// Copyright 2021 Shiwen Cheng. All rights reserved.
// Use of this source code is governed by a MIT
// license that can be found in the LICENSE file.

package backend

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chengshiwen/influx-proxy/util"
	"github.com/influxdata/influxdb1-client/models"
)

type Proxy struct {
	lock    sync.RWMutex
	circles []*Circle
	dbSet   util.Set
}

func NewProxy(cfg *ProxyConfig) (ip *Proxy) {
	err := util.MakeDir(cfg.DataDir)
	if err != nil {
		log.Fatalf("create data dir error: %s", err)
		return
	}
	ip = &Proxy{
		circles: newCircles(cfg),
		dbSet:   newDBSet(cfg),
	}
	rand.Seed(time.Now().UnixNano())
	return
}

func newCircles(cfg *ProxyConfig) []*Circle {
	circles := make([]*Circle, len(cfg.Circles))
	for idx, circfg := range cfg.Circles {
		circles[idx] = NewCircle(circfg, cfg, idx)
	}
	return circles
}

func newDBSet(cfg *ProxyConfig) util.Set {
	dbSet := util.NewSet()
	for _, db := range cfg.DBList {
		dbSet.Add(db)
	}
	return dbSet
}

func (ip *Proxy) ReloadConfig(cfg *ProxyConfig) error {
	err := util.MakeDir(cfg.DataDir)
	if err != nil {
		return err
	}

	circles := newCircles(cfg)
	dbSet := newDBSet(cfg)

	ip.lock.Lock()
	oldCircles := ip.circles
	ip.circles = circles
	ip.dbSet = dbSet
	ip.lock.Unlock()

	for _, c := range oldCircles {
		c.Close()
	}
	return nil
}

func (ip *Proxy) Circles() []*Circle {
	ip.lock.RLock()
	defer ip.lock.RUnlock()
	return ip.circles
}

func (ip *Proxy) Circle(idx int) *Circle {
	ip.lock.RLock()
	defer ip.lock.RUnlock()
	if idx < 0 || idx >= len(ip.circles) {
		return nil
	}
	return ip.circles[idx]
}

func GetKey(db, meas string) string {
	var b strings.Builder
	b.Grow(len(db) + len(meas) + 1)
	b.WriteString(db)
	b.WriteString(",")
	b.WriteString(meas)
	return b.String()
}

func (ip *Proxy) GetBackends(key string) []*Backend {
	circles := ip.Circles()
	backends := make([]*Backend, len(circles))
	for i, circle := range circles {
		backends[i] = circle.GetBackend(key)
	}
	return backends
}

func (ip *Proxy) GetAllBackends() []*Backend {
	circles := ip.Circles()
	capacity := 0
	for _, circle := range circles {
		capacity += len(circle.Backends())
	}
	backends := make([]*Backend, 0, capacity)
	for _, circle := range circles {
		backends = append(backends, circle.Backends()...)
	}
	return backends
}

func (ip *Proxy) GetHealth(stats bool) []interface{} {
	var wg sync.WaitGroup
	circles := ip.Circles()
	health := make([]interface{}, len(circles))
	for i, c := range circles {
		wg.Add(1)
		go func(i int, c *Circle) {
			defer wg.Done()
			health[i] = c.GetHealth(stats)
		}(i, c)
	}
	wg.Wait()
	return health
}

func (ip *Proxy) IsForbiddenDB(db string) bool {
	ip.lock.RLock()
	defer ip.lock.RUnlock()
	return db == "_internal" || (len(ip.dbSet) > 0 && !ip.dbSet[db])
}

func (ip *Proxy) Query(w http.ResponseWriter, req *http.Request) (body []byte, err error) {
	q := strings.TrimSpace(req.FormValue("q"))
	if q == "" {
		return nil, ErrEmptyQuery
	}

	tokens, check, from := CheckQuery(q)
	if !check {
		return nil, ErrIllegalQL
	}

	checkDb, showDb, alterDb, db := CheckDatabaseFromTokens(tokens)
	if !checkDb {
		db, _ = GetDatabaseFromTokens(tokens)
		if db == "" {
			db = req.FormValue("db")
		}
	}
	if !showDb {
		if db == "" {
			return nil, ErrDatabaseNotFound
		}
		if ip.IsForbiddenDB(db) {
			return nil, fmt.Errorf("database forbidden: %s", db)
		}
	}

	selectOrShow := CheckSelectOrShowFromTokens(tokens)
	if selectOrShow && from {
		return QueryFromQL(w, req, ip, tokens, db)
	} else if selectOrShow && !from {
		return QueryShowQL(w, req, ip, tokens)
	} else if CheckDeleteOrDropMeasurementFromTokens(tokens) {
		return QueryDeleteOrDropQL(w, req, ip, tokens, db)
	} else if alterDb || CheckRetentionPolicyFromTokens(tokens) {
		return QueryAlterQL(w, req, ip)
	}
	return nil, ErrIllegalQL
}

func (ip *Proxy) Write(p []byte, db, rp, precision string) (err error) {
	buf := bytes.NewBuffer(p)
	var line []byte
	for {
		line, err = buf.ReadBytes('\n')
		switch err {
		default:
			log.Printf("error: %s", err)
			return
		case io.EOF, nil:
			err = nil
		}
		if len(line) == 0 {
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		ip.WriteRow(line, db, rp, precision)
	}
	return
}

func (ip *Proxy) WriteRow(line []byte, db, rp, precision string) {
	nanoLine := AppendNano(line, precision)
	meas, err := ScanKey(nanoLine)
	if err != nil {
		log.Printf("scan key error: %s", err)
		return
	}
	if !RapidCheck(nanoLine[len(meas):]) {
		log.Printf("invalid format, drop data: %s %s %s %s", db, rp, precision, string(line))
		return
	}

	key := GetKey(db, meas)
	backends := ip.GetBackends(key)
	if len(backends) == 0 {
		log.Printf("write data error: can't get backends, db: %s, meas: %s", db, meas)
		return
	}

	point := &LinePoint{db, rp, nanoLine}
	for _, be := range backends {
		err = be.WritePoint(point)
		if err != nil {
			log.Printf("write data to buffer error: %s, %s, %s, %s, %s, %s", err, be.Url, db, rp, precision, string(line))
		}
	}
}

func (ip *Proxy) WritePoints(points []models.Point, db, rp string) error {
	var err error
	for _, pt := range points {
		meas := string(pt.Name())
		key := GetKey(db, meas)
		backends := ip.GetBackends(key)
		if len(backends) == 0 {
			log.Printf("write point error: can't get backends, db: %s, meas: %s", db, meas)
			err = ErrEmptyBackends
			continue
		}

		point := &LinePoint{db, rp, []byte(pt.String())}
		for _, be := range backends {
			err = be.WritePoint(point)
			if err != nil {
				log.Printf("write point to buffer error: %s, %s, %s, %s, %s", err, be.Url, db, rp, pt.String())
			}
		}
	}
	return err
}

func (ip *Proxy) ReadProm(w http.ResponseWriter, req *http.Request, db, metric string) (err error) {
	return ReadPromQL(w, req, ip, db, metric)
}

func (ip *Proxy) Close() {
	for _, c := range ip.Circles() {
		c.Close()
	}
}
