package backend

import (
	"bytes"
	"io"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/panjf2000/ants/v2"
)

type CacheBuffer struct {
	Buffer  *bytes.Buffer
	Counter int
}

type Backend struct {
	*HttpBackend
	fb   *FileBackend
	pool *ants.Pool

	flushSize       int
	flushTime       int
	rewriteInterval int
	rewriteTicker   *time.Ticker
	rewriteRunning  bool
	chWrite         chan *LinePoint
	chTimer         <-chan time.Time
	buffers         map[string]*CacheBuffer
	wg              sync.WaitGroup
}

func NewBackend(cfg *BackendConfig, pxcfg *ProxyConfig) (ib *Backend) {
	ib = &Backend{
		HttpBackend:     NewHttpBackend(cfg, pxcfg),
		flushSize:       pxcfg.FlushSize,
		flushTime:       pxcfg.FlushTime,
		rewriteInterval: pxcfg.RewriteInterval,
		rewriteTicker:   time.NewTicker(time.Duration(pxcfg.RewriteInterval) * time.Second),
		rewriteRunning:  false,
		chWrite:         make(chan *LinePoint, 16),
		buffers:         make(map[string]*CacheBuffer),
	}

	var err error
	ib.fb, err = NewFileBackend(cfg.Name, pxcfg.DataDir)
	if err != nil {
		panic(err)
	}
	ib.pool, err = ants.NewPool(pxcfg.ConnPoolSize)
	if err != nil {
		panic(err)
	}

	go ib.worker()
	return
}

func NewSimpleBackend(cfg *BackendConfig) *Backend {
	return &Backend{HttpBackend: NewSimpleHttpBackend(cfg)}
}

func (ib *Backend) worker() {
	for {
		select {
		case p, ok := <-ib.chWrite:
			if !ok {
				// closed
				ib.Flush()
				ib.wg.Wait()
				ib.HttpBackend.Close()
				ib.fb.Close()
				return
			}
			ib.WriteBuffer(p)

		case <-ib.chTimer:
			ib.Flush()

		case <-ib.rewriteTicker.C:
			ib.RewriteIdle()
		}
	}
}

func (ib *Backend) WritePoint(point *LinePoint) (err error) {
	ib.chWrite <- point
	return
}

func (ib *Backend) WriteBuffer(point *LinePoint) (err error) {
	db, line := point.Db, point.Line
	cb, ok := ib.buffers[db]
	if !ok {
		ib.buffers[db] = &CacheBuffer{Buffer: &bytes.Buffer{}}
		cb = ib.buffers[db]
	}
	cb.Counter++
	if cb.Buffer == nil {
		cb.Buffer = &bytes.Buffer{}
	}
	n, err := cb.Buffer.Write(line)
	if err != nil {
		log.Printf("buffer write error: %s", err)
		return
	}
	if n != len(line) {
		err = io.ErrShortWrite
		log.Printf("buffer write error: %s", err)
		return
	}
	if line[len(line)-1] != '\n' {
		_, err = cb.Buffer.Write([]byte{'\n'})
		if err != nil {
			log.Printf("buffer write error: %s", err)
			return
		}
	}

	switch {
	case cb.Counter >= ib.flushSize:
		err = ib.FlushBuffer(db)
		if err != nil {
			return
		}
	case ib.chTimer == nil:
		ib.chTimer = time.After(time.Duration(ib.flushTime) * time.Second)
	}
	return
}

func (ib *Backend) FlushBuffer(db string) (err error) {
	cb := ib.buffers[db]
	if cb.Buffer == nil {
		return
	}
	p := cb.Buffer.Bytes()
	cb.Buffer = nil
	cb.Counter = 0
	if len(p) == 0 {
		return
	}

	ib.wg.Add(1)
	ib.pool.Submit(func() {
		defer ib.wg.Done()
		var buf bytes.Buffer
		err = Compress(&buf, p)
		if err != nil {
			log.Print("compress buffer error: ", err)
			return
		}

		p = buf.Bytes()

		if ib.Active {
			err = ib.WriteCompressed(db, p)
			switch err {
			case nil:
				return
			case ErrBadRequest:
				log.Printf("bad request, drop all data")
				return
			case ErrNotFound:
				log.Printf("bad backend, drop all data")
				return
			default:
				log.Printf("write http error: %s %s, length: %d", ib.Url, db, len(p))
			}
		}

		b := bytes.Join([][]byte{[]byte(url.QueryEscape(db)), p}, []byte{' '})
		err = ib.fb.Write(b)
		if err != nil {
			log.Printf("write db and data to file error with db: %s, length: %d error: %s", db, len(p), err)
			return
		}
	})
	return
}

func (ib *Backend) Flush() {
	ib.chTimer = nil
	for db := range ib.buffers {
		if ib.buffers[db].Counter > 0 {
			err := ib.FlushBuffer(db)
			if err != nil {
				log.Printf("flush buffer background error: %s %s", ib.Url, err)
			}
		}
	}
}

func (ib *Backend) RewriteIdle() {
	if !ib.rewriteRunning && ib.fb.IsData() {
		ib.rewriteRunning = true
		go ib.RewriteLoop()
	}
}

func (ib *Backend) RewriteLoop() {
	for ib.fb.IsData() {
		if !ib.Active {
			time.Sleep(time.Duration(ib.rewriteInterval) * time.Second)
			continue
		}
		err := ib.Rewrite()
		if err != nil {
			time.Sleep(time.Duration(ib.rewriteInterval) * time.Second)
			continue
		}
	}
	ib.rewriteRunning = false
}

func (ib *Backend) Rewrite() (err error) {
	b, err := ib.fb.Read()
	if err != nil {
		log.Print("rewrite read file error: ", err)
		return
	}
	if b == nil {
		return
	}

	p := bytes.SplitN(b, []byte{' '}, 2)
	if len(p) < 2 {
		log.Print("rewrite read invalid data with length: ", len(p))
		return
	}
	db, err := url.QueryUnescape(string(p[0]))
	if err != nil {
		log.Print("rewrite db unescape error: ", err)
		return
	}
	err = ib.WriteCompressed(db, p[1])

	switch err {
	case nil:
	case ErrBadRequest:
		log.Printf("bad request, drop all data")
		err = nil
	case ErrNotFound:
		log.Printf("bad backend, drop all data")
		err = nil
	default:
		log.Printf("rewrite http error: %s %s, length: %d", ib.Url, db, len(p[1]))

		err = ib.fb.RollbackMeta()
		if err != nil {
			log.Printf("rollback meta error: %s", err)
		}
		return
	}

	err = ib.fb.UpdateMeta()
	if err != nil {
		log.Printf("update meta error: %s", err)
	}
	return
}

func (ib *Backend) Close() {
	ib.pool.Release()
	close(ib.chWrite)
}

func (ib *Backend) GetHealth(ic *Circle) map[string]interface{} {
	return map[string]interface{}{
		"name":    ib.Name,
		"url":     ib.Url,
		"active":  ib.Active,
		"backlog": ib.fb.IsData(),
		"rewrite": ib.rewriteRunning,
		"stats":   ib.GetStats(ic),
	}
}

func (ib *Backend) GetStats(ic *Circle) map[string]map[string]int {
	load := make(map[string]map[string]int)
	dbs := ib.GetDatabases()
	for _, db := range dbs {
		inplace, incorrect := 0, 0
		measurements := ib.GetMeasurements(db)
		for _, meas := range measurements {
			key := GetKey(db, meas)
			nb := ic.GetBackend(key)
			if nb.Url == ib.Url {
				inplace++
			} else {
				incorrect++
			}
		}
		load[db] = map[string]int{"measurements": len(measurements), "inplace": inplace, "incorrect": incorrect}
	}
	return load
}
