// Copyright 2021 Shiwen Cheng. All rights reserved.
// Use of this source code is governed by a MIT
// license that can be found in the LICENSE file.

package service

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/pprof"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/chengshiwen/influx-proxy/backend"
	"github.com/chengshiwen/influx-proxy/service/prometheus"
	"github.com/chengshiwen/influx-proxy/service/prometheus/remote"
	"github.com/chengshiwen/influx-proxy/transfer"
	"github.com/chengshiwen/influx-proxy/util"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	ErrInvalidTick    = errors.New("invalid tick, require non-negative integer")
	ErrInvalidWorker  = errors.New("invalid worker, require positive integer")
	ErrInvalidBatch   = errors.New("invalid batch, require positive integer")
	ErrInvalidLimit   = errors.New("invalid limit, require positive integer")
	ErrInvalidHaAddrs = errors.New("invalid ha_addrs, require at least two addresses as <host:port>, comma-separated")
)

type HttpService struct { // nolint:golint
	cfg          *backend.ProxyConfig
	ip           *backend.Proxy
	tx           *transfer.Transfer
	username     atomic.Value
	password     atomic.Value
	authEncrypt  atomic.Value
	writeTracing atomic.Value
	queryTracing atomic.Value
	pprofEnabled atomic.Value
}

func NewHttpService(cfg *backend.ProxyConfig) (hs *HttpService) { // nolint:golint
	ip := backend.NewProxy(cfg)
	hs = &HttpService{
		ip: ip,
		tx: transfer.NewTransfer(cfg, ip.Circles()),
	}
	hs.setConfig(cfg)
	return
}

func (hs *HttpService) setConfig(cfg *backend.ProxyConfig) {
	hs.cfg = cfg
	hs.username.Store(cfg.Username)
	hs.password.Store(cfg.Password)
	hs.authEncrypt.Store(cfg.AuthEncrypt)
	hs.writeTracing.Store(cfg.WriteTracing)
	hs.queryTracing.Store(cfg.QueryTracing)
	hs.pprofEnabled.Store(cfg.PprofEnabled)
}

func (hs *HttpService) Register(mux *http.ServeMux) {
	mux.HandleFunc("/ping", hs.HandlerPing)
	mux.HandleFunc("/query", hs.HandlerQuery)
	mux.HandleFunc("/write", hs.HandlerWrite)
	mux.HandleFunc("/reload", hs.HandlerReload)
	mux.HandleFunc("/health", hs.HandlerHealth)
	mux.HandleFunc("/replica", hs.HandlerReplica)
	mux.HandleFunc("/encrypt", hs.HandlerEncrypt)
	mux.HandleFunc("/decrypt", hs.HandlerDecrypt)
	mux.HandleFunc("/rebalance", hs.HandlerRebalance)
	mux.HandleFunc("/recovery", hs.HandlerRecovery)
	mux.HandleFunc("/resync", hs.HandlerResync)
	mux.HandleFunc("/cleanup", hs.HandlerCleanup)
	mux.HandleFunc("/transfer/state", hs.HandlerTransferState)
	mux.HandleFunc("/transfer/stats", hs.HandlerTransferStats)
	mux.HandleFunc("/api/v1/prom/read", hs.HandlerPromRead)
	mux.HandleFunc("/api/v1/prom/write", hs.HandlerPromWrite)
	mux.HandleFunc("/metrics", promhttp.Handler().ServeHTTP)
	if hs.pprofEnabled.Load().(bool) {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	}
}

func (hs *HttpService) HandlerPing(w http.ResponseWriter, req *http.Request) {
	hs.WriteHeader(w, 204)
}

func (hs *HttpService) HandlerQuery(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "GET", "POST") {
		return
	}

	db := req.FormValue("db")
	q := req.FormValue("q")
	body, err := hs.ip.Query(w, req)
	if err != nil {
		log.Printf("query error: %s, query: %s %s %s, client: %s", err, req.Method, db, q, req.RemoteAddr)
		hs.WriteError(w, req, 400, err.Error())
		return
	}
	hs.WriteBody(w, body)
	if hs.isQueryTracing() {
		log.Printf("query: %s %s %s, client: %s", req.Method, db, q, req.RemoteAddr)
	}
}

func (hs *HttpService) HandlerWrite(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "POST") {
		return
	}

	precision := req.URL.Query().Get("precision")
	switch precision {
	case "", "n", "ns", "u", "ms", "s", "m", "h":
		// it's valid
		if precision == "" {
			precision = "ns"
		}
	default:
		hs.WriteError(w, req, 400, fmt.Sprintf("invalid precision %q (use n, ns, u, ms, s, m or h)", precision))
		return
	}

	db, err := hs.queryDB(req, false)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}
	rp := req.URL.Query().Get("rp")

	body := req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		b, err := gzip.NewReader(body)
		if err != nil {
			hs.WriteError(w, req, 400, "unable to decode gzip body")
			return
		}
		defer b.Close()
		body = b
	}
	p, err := ioutil.ReadAll(body)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	err = hs.ip.Write(p, db, rp, precision)
	if err == nil {
		hs.WriteHeader(w, 204)
	}
	if hs.isWriteTracing() {
		log.Printf("write: %s %s %s %s, client: %s", db, rp, precision, p, req.RemoteAddr)
	}
}

func (hs *HttpService) HandlerReload(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "GET", "POST") {
		return
	}

	nfc, err := backend.ReadFileConfig()
	if err != nil {
		log.Printf("read config error: %s", err)
		hs.WriteError(w, req, 500, err.Error())
		return
	}

	if reflect.DeepEqual(nfc, hs.cfg) {
		log.Printf("config not changed")
		hs.WriteError(w, req, 500, "config not changed")
		return
	}
	// Items that cannot be modified in runtime: listen_addr, idle_timeout, https_enabled, https_cert, https_key
	nfc.KeepInRuntime(hs.cfg)
	if reflect.DeepEqual(nfc, hs.cfg) {
		log.Printf("config changed, but ignored, and this change should be rolled back: listen_addr, idle_timeout, https_enabled, https_cert, https_key")
		hs.WriteError(w, req, 500, "config changed, but ignored: listen_addr, idle_timeout, https_enabled, https_cert, https_key")
		return
	}

	log.Printf("config reloading")
	err = hs.ip.ReloadConfig(nfc)
	if err != nil {
		log.Printf("config reload error: %s", err)
		hs.WriteError(w, req, 500, err.Error())
		return
	}
	hs.tx.ReloadConfig(nfc, hs.ip.Circles())
	hs.setConfig(nfc)
	nfc.PrintSummary()
	log.Printf("config reloaded")

	hs.WriteText(w, 200, nfc.String())
}

func (hs *HttpService) HandlerHealth(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "GET") {
		return
	}
	stats := req.URL.Query().Get("stats") == "true"
	hs.Write(w, req, 200, hs.ip.GetHealth(stats))
}

func (hs *HttpService) HandlerReplica(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "GET") {
		return
	}

	db := req.URL.Query().Get("db")
	meas := req.URL.Query().Get("meas")
	if db != "" && meas != "" {
		key := backend.GetKey(db, meas)
		backends := hs.ip.GetBackends(key)
		data := make([]map[string]interface{}, len(backends))
		for i, b := range backends {
			c := hs.ip.Circle(i)
			data[i] = map[string]interface{}{
				"backend": map[string]string{"name": b.Name, "url": b.Url},
				"circle":  map[string]interface{}{"id": c.CircleId, "name": c.Name},
			}
		}
		hs.Write(w, req, 200, data)
	} else {
		hs.WriteError(w, req, 400, "invalid db or meas")
	}
}

func (hs *HttpService) HandlerEncrypt(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethod(w, req, "GET") {
		return
	}
	text := req.URL.Query().Get("text")
	encrypt := util.AesEncrypt(text)
	hs.WriteText(w, 200, encrypt)
}

func (hs *HttpService) HandlerDecrypt(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethod(w, req, "GET") {
		return
	}
	key := req.URL.Query().Get("key")
	text := req.URL.Query().Get("text")
	if !util.CheckCipherKey(key) {
		hs.WriteError(w, req, 400, "invalid key")
		return
	}
	decrypt := util.AesDecrypt(text)
	hs.WriteText(w, 200, decrypt)
}

func (hs *HttpService) HandlerRebalance(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "POST") {
		return
	}

	circleId, err := hs.formCircleId(req, "circle_id") // nolint:golint
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}
	operation := req.FormValue("operation")
	if operation != "add" && operation != "rm" {
		hs.WriteError(w, req, 400, "invalid operation")
		return
	}

	var backends []*backend.Backend
	if operation == "rm" {
		var body struct {
			Backends []*backend.BackendConfig `json:"backends"`
		}
		decoder := json.NewDecoder(req.Body)
		err := decoder.Decode(&body)
		if err != nil {
			hs.WriteError(w, req, 400, "invalid backends from body")
			return
		}
		for _, bkcfg := range body.Backends {
			backends = append(backends, backend.NewSimpleBackend(bkcfg))
			hs.tx.CircleState(circleId).Stats[bkcfg.Url] = &transfer.Stats{}
		}
	}
	backends = append(backends, hs.ip.Circle(circleId).Backends()...)

	if hs.tx.CircleState(circleId).Transferring {
		hs.WriteText(w, 400, fmt.Sprintf("circle %d is transferring", circleId))
		return
	}
	if hs.tx.Resyncing {
		hs.WriteText(w, 400, "proxy is resyncing")
		return
	}

	err = hs.setParam(req)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	dbs := hs.formValues(req, "dbs")
	go hs.tx.Rebalance(circleId, backends, dbs)
	hs.WriteText(w, 202, "accepted")
}

func (hs *HttpService) HandlerRecovery(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "POST") {
		return
	}

	fromCircleId, err := hs.formCircleId(req, "from_circle_id") // nolint:golint
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}
	toCircleId, err := hs.formCircleId(req, "to_circle_id") // nolint:golint
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}
	if fromCircleId == toCircleId {
		hs.WriteError(w, req, 400, "from_circle_id and to_circle_id cannot be same")
		return
	}

	if hs.tx.CircleState(fromCircleId).Transferring || hs.tx.CircleState(toCircleId).Transferring {
		hs.WriteText(w, 400, fmt.Sprintf("circle %d or %d is transferring", fromCircleId, toCircleId))
		return
	}
	if hs.tx.Resyncing {
		hs.WriteText(w, 400, "proxy is resyncing")
		return
	}

	err = hs.setParam(req)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	backendUrls := hs.formValues(req, "backend_urls")
	dbs := hs.formValues(req, "dbs")
	go hs.tx.Recovery(fromCircleId, toCircleId, backendUrls, dbs)
	hs.WriteText(w, 202, "accepted")
}

func (hs *HttpService) HandlerResync(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "POST") {
		return
	}

	tick, err := hs.formTick(req)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	for _, cs := range hs.tx.CircleStates() {
		if cs.Transferring {
			hs.WriteText(w, 400, fmt.Sprintf("circle %d is transferring", cs.CircleId))
			return
		}
	}
	if hs.tx.Resyncing {
		hs.WriteText(w, 400, "proxy is resyncing")
		return
	}

	err = hs.setParam(req)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	dbs := hs.formValues(req, "dbs")
	go hs.tx.Resync(dbs, tick)
	hs.WriteText(w, 202, "accepted")
}

func (hs *HttpService) HandlerCleanup(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "POST") {
		return
	}

	circleId, err := hs.formCircleId(req, "circle_id") // nolint:golint
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	if hs.tx.CircleState(circleId).Transferring {
		hs.WriteText(w, 400, fmt.Sprintf("circle %d is transferring", circleId))
		return
	}
	if hs.tx.Resyncing {
		hs.WriteText(w, 400, "proxy is resyncing")
		return
	}

	err = hs.setParam(req)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	go hs.tx.Cleanup(circleId)
	hs.WriteText(w, 202, "accepted")
}

func (hs *HttpService) HandlerTransferState(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "GET", "POST") {
		return
	}

	if req.Method == "GET" {
		data := make([]map[string]interface{}, len(hs.tx.CircleStates()))
		for k, cs := range hs.tx.CircleStates() {
			data[k] = map[string]interface{}{
				"id":           cs.CircleId,
				"name":         cs.Name,
				"transferring": cs.Transferring,
			}
		}
		state := map[string]interface{}{"resyncing": hs.tx.Resyncing, "circles": data}
		hs.Write(w, req, 200, state)
		return
	} else if req.Method == "POST" {
		state := make(map[string]interface{})
		if req.FormValue("resyncing") != "" {
			resyncing, err := hs.formBool(req, "resyncing")
			if err != nil {
				hs.WriteError(w, req, 400, "illegal resyncing")
				return
			}
			hs.tx.Resyncing = resyncing
			state["resyncing"] = resyncing
		}
		if req.FormValue("circle_id") != "" || req.FormValue("transferring") != "" {
			circleId, err := hs.formCircleId(req, "circle_id") // nolint:golint
			if err != nil {
				hs.WriteError(w, req, 400, err.Error())
				return
			}
			transferring, err := hs.formBool(req, "transferring")
			if err != nil {
				hs.WriteError(w, req, 400, "illegal transferring")
				return
			}
			cs := hs.tx.CircleState(circleId)
			cs.Transferring = transferring
			cs.SetTransferIn(transferring)
			state["circle"] = map[string]interface{}{
				"id":           cs.CircleId,
				"name":         cs.Name,
				"transferring": cs.Transferring,
			}
		}
		if len(state) == 0 {
			hs.WriteError(w, req, 400, "missing query parameter")
			return
		}
		hs.Write(w, req, 200, state)
		return
	}
}

func (hs *HttpService) HandlerTransferStats(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "GET") {
		return
	}

	circleId, err := hs.formCircleId(req, "circle_id") // nolint:golint
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	statsType := req.FormValue("type")
	if statsType == "rebalance" || statsType == "recovery" || statsType == "resync" || statsType == "cleanup" {
		hs.Write(w, req, 200, hs.tx.CircleState(circleId).Stats)
	} else {
		hs.WriteError(w, req, 400, "invalid stats type")
	}
}

func (hs *HttpService) HandlerPromRead(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "POST") {
		return
	}

	db, err := hs.queryDB(req, true)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	compressed, err := ioutil.ReadAll(req.Body)
	if err != nil {
		hs.WriteError(w, req, 500, err.Error())
		return
	}

	reqBuf, err := snappy.Decode(nil, compressed)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	var readReq remote.ReadRequest
	if err = proto.Unmarshal(reqBuf, &readReq); err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}
	if len(readReq.Queries) != 1 {
		err = errors.New("prometheus read endpoint currently only supports one query at a time")
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	var metric string
	q := readReq.Queries[0]
	for _, m := range q.Matchers {
		if m.Name == "__name__" {
			metric = m.Value
		}
	}
	if metric == "" {
		log.Printf("prometheus query: %v", q)
		err = errors.New("prometheus metric not found")
		hs.WriteError(w, req, 400, err.Error())
	}

	req.Body = ioutil.NopCloser(bytes.NewBuffer(compressed))
	err = hs.ip.ReadProm(w, req, db, metric)
	if err != nil {
		log.Printf("prometheus read error: %s, query: %s %s %v, client: %s", err, req.Method, db, q, req.RemoteAddr)
		hs.WriteError(w, req, 400, err.Error())
		return
	}
	if hs.isQueryTracing() {
		log.Printf("prometheus read: %s %s %v, client: %s", req.Method, db, q, req.RemoteAddr)
	}
}

func (hs *HttpService) HandlerPromWrite(w http.ResponseWriter, req *http.Request) {
	if !hs.checkMethodAndAuth(w, req, "POST") {
		return
	}

	db, err := hs.queryDB(req, false)
	if err != nil {
		hs.WriteError(w, req, 400, err.Error())
		return
	}
	rp := req.URL.Query().Get("rp")

	body := req.Body
	var bs []byte
	if req.ContentLength > 0 {
		// This will just be an initial hint for the reader, as the
		// bytes.Buffer will grow as needed when ReadFrom is called
		bs = make([]byte, 0, req.ContentLength)
	}
	buf := bytes.NewBuffer(bs)

	_, err = buf.ReadFrom(body)
	if err != nil {
		if hs.isWriteTracing() {
			log.Printf("prom write handler unable to read bytes from request body")
		}
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	reqBuf, err := snappy.Decode(nil, buf.Bytes())
	if err != nil {
		if hs.isWriteTracing() {
			log.Printf("prom write handler unable to snappy decode from request body, error: %s", err)
		}
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	// Convert the Prometheus remote write request to Influx Points
	var writeReq remote.WriteRequest
	if err = proto.Unmarshal(reqBuf, &writeReq); err != nil {
		if hs.isWriteTracing() {
			log.Printf("prom write handler unable to unmarshal from snappy decoded bytes, error: %s", err)
		}
		hs.WriteError(w, req, 400, err.Error())
		return
	}

	points, err := prometheus.WriteRequestToPoints(&writeReq)
	if err != nil {
		if hs.isWriteTracing() {
			log.Printf("prom write handler, error: %s", err)
		}
		// Check if the error was from something other than dropping invalid values.
		if _, ok := err.(prometheus.DroppedValuesError); !ok {
			hs.WriteError(w, req, 400, err.Error())
			return
		}
	}

	// Write points.
	err = hs.ip.WritePoints(points, db, rp)
	if err == nil {
		hs.WriteHeader(w, 204)
	}
}

func (hs *HttpService) isWriteTracing() bool {
	return hs.writeTracing.Load().(bool)
}

func (hs *HttpService) isQueryTracing() bool {
	return hs.queryTracing.Load().(bool)
}

func (hs *HttpService) Write(w http.ResponseWriter, req *http.Request, status int, data interface{}) {
	if status >= 400 {
		hs.WriteError(w, req, status, data.(string))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	hs.WriteHeader(w, status)
	pretty := req.URL.Query().Get("pretty") == "true"
	w.Write(util.MarshalJSON(data, pretty))
}

func (hs *HttpService) WriteError(w http.ResponseWriter, req *http.Request, status int, err string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Influxdb-Error", err)
	hs.WriteHeader(w, status)
	rsp := backend.ResponseFromError(err)
	pretty := req.URL.Query().Get("pretty") == "true"
	w.Write(util.MarshalJSON(rsp, pretty))
}

func (hs *HttpService) WriteBody(w http.ResponseWriter, body []byte) {
	hs.WriteHeader(w, 200)
	w.Write(body)
}

func (hs *HttpService) WriteText(w http.ResponseWriter, status int, text string) {
	hs.WriteHeader(w, status)
	w.Write([]byte(text + "\n"))
}

func (hs *HttpService) WriteHeader(w http.ResponseWriter, status int) {
	w.Header().Set("X-Influxdb-Version", backend.Version)
	w.WriteHeader(status)
}

func (hs *HttpService) checkMethodAndAuth(w http.ResponseWriter, req *http.Request, methods ...string) bool {
	return hs.checkMethod(w, req, methods...) && hs.checkAuth(w, req)
}

func (hs *HttpService) checkMethod(w http.ResponseWriter, req *http.Request, methods ...string) bool {
	for _, method := range methods {
		if req.Method == method {
			return true
		}
	}
	hs.WriteError(w, req, 405, "method not allow")
	return false
}

func (hs *HttpService) checkAuth(w http.ResponseWriter, req *http.Request) bool {
	username := hs.username.Load().(string)
	password := hs.password.Load().(string)
	if username == "" && password == "" {
		return true
	}
	query := req.URL.Query()
	u, p := query.Get("u"), query.Get("p")
	if hs.transAuth(u) == username && hs.transAuth(p) == password {
		return true
	}
	u, p, ok := req.BasicAuth()
	if ok && hs.transAuth(u) == username && hs.transAuth(p) == password {
		return true
	}
	hs.WriteError(w, req, 401, "authentication failed")
	return false
}

func (hs *HttpService) transAuth(text string) string {
	if hs.authEncrypt.Load().(bool) {
		return util.AesEncrypt(text)
	}
	return text
}

func (hs *HttpService) queryDB(req *http.Request, form bool) (string, error) {
	var db string
	if form {
		db = req.FormValue("db")
	} else {
		db = req.URL.Query().Get("db")
	}
	if db == "" {
		return db, errors.New("database not found")
	}
	if hs.ip.IsForbiddenDB(db) {
		return db, fmt.Errorf("database forbidden: %s", db)
	}
	return db, nil
}

func (hs *HttpService) formValues(req *http.Request, key string) []string {
	var values []string
	str := strings.Trim(req.FormValue(key), ", ")
	if str != "" {
		values = strings.Split(str, ",")
	}
	return values
}

func (hs *HttpService) formBool(req *http.Request, key string) (bool, error) {
	return strconv.ParseBool(req.FormValue(key))
}

func (hs *HttpService) formTick(req *http.Request) (int64, error) {
	str := strings.TrimSpace(req.FormValue("tick"))
	if str == "" {
		return 0, nil
	}
	tick, err := strconv.ParseInt(str, 10, 64)
	if err != nil || tick < 0 {
		return 0, ErrInvalidTick
	}
	return tick, nil
}

func (hs *HttpService) formCircleId(req *http.Request, key string) (int, error) { // nolint:golint
	circleId, err := strconv.Atoi(req.FormValue(key)) // nolint:golint
	if err != nil || circleId < 0 || circleId >= len(hs.ip.Circles()) {
		return circleId, fmt.Errorf("invalid %s", key)
	}
	return circleId, nil
}

func (hs *HttpService) setParam(req *http.Request) error {
	var err error
	err = hs.setWorker(req)
	if err != nil {
		return err
	}
	err = hs.setBatch(req)
	if err != nil {
		return err
	}
	err = hs.setLimit(req)
	if err != nil {
		return err
	}
	err = hs.setHaAddrs(req)
	if err != nil {
		return err
	}
	return nil
}

func (hs *HttpService) setWorker(req *http.Request) error {
	str := strings.TrimSpace(req.FormValue("worker"))
	if str != "" {
		worker, err := strconv.Atoi(str)
		if err != nil || worker <= 0 {
			return ErrInvalidWorker
		}
		hs.tx.Worker = worker
	} else {
		hs.tx.Worker = transfer.DefaultWorker
	}
	return nil
}

func (hs *HttpService) setBatch(req *http.Request) error {
	str := strings.TrimSpace(req.FormValue("batch"))
	if str != "" {
		batch, err := strconv.Atoi(str)
		if err != nil || batch <= 0 {
			return ErrInvalidBatch
		}
		hs.tx.Batch = batch
	} else {
		hs.tx.Batch = transfer.DefaultBatch
	}
	return nil
}

func (hs *HttpService) setLimit(req *http.Request) error {
	str := strings.TrimSpace(req.FormValue("limit"))
	if str != "" {
		limit, err := strconv.Atoi(str)
		if err != nil || limit <= 0 {
			return ErrInvalidLimit
		}
		hs.tx.Limit = limit
	} else {
		hs.tx.Limit = transfer.DefaultLimit
	}
	return nil
}

func (hs *HttpService) setHaAddrs(req *http.Request) error {
	haAddrs := hs.formValues(req, "ha_addrs")
	if len(haAddrs) > 1 {
		r, _ := regexp.Compile(`^[\w-.]+:\d{1,5}$`)
		for _, addr := range haAddrs {
			if !r.MatchString(addr) {
				return ErrInvalidHaAddrs
			}
		}
		hs.tx.HaAddrs = haAddrs
	} else if len(haAddrs) == 1 {
		return ErrInvalidHaAddrs
	}
	return nil
}
