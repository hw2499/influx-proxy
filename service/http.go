package service

import (
    "bytes"
    "compress/gzip"
    "encoding/json"
    "errors"
    "fmt"
    "github.com/chengshiwen/influx-proxy/backend"
    "github.com/chengshiwen/influx-proxy/config"
    "github.com/chengshiwen/influx-proxy/util"
    "io/ioutil"
    "log"
    "math/rand"
    "net/http"
    "net/http/pprof"
    "runtime"
    "strconv"
    "strings"
    "sync"
    "time"
)

type HttpService struct {
    *backend.Proxy
}

func (hs *HttpService) Register(mux *http.ServeMux) {
    mux.HandleFunc("/ping", hs.HandlerPing)
    mux.HandleFunc("/query", hs.HandlerQuery)
    mux.HandleFunc("/write", hs.HandlerWrite)
    mux.HandleFunc("/health", hs.HandlerHealth)
    mux.HandleFunc("/replica", hs.HandlerReplica)
    mux.HandleFunc("/encrypt", hs.HandlerEncrypt)
    mux.HandleFunc("/decrypt", hs.HandlerDencrypt)
    mux.HandleFunc("/migrating", hs.HandlerMigrating)
    mux.HandleFunc("/rebalance", hs.HandlerRebalance)
    mux.HandleFunc("/recovery", hs.HandlerRecovery)
    mux.HandleFunc("/resync", hs.HandlerResync)
    mux.HandleFunc("/clear", hs.HandlerClear)
    mux.HandleFunc("/stats", hs.HandlerStats)
    mux.HandleFunc("/debug/pprof/", pprof.Index)
    mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
    return
}

func (hs *HttpService) HandlerPing(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)
    w.WriteHeader(204)
    return
}

func (hs *HttpService) HandlerQuery(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodGet && req.Method != http.MethodPost {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    if !hs.checkAuth(req) {
        w.WriteHeader(401)
        w.Write([]byte("authentication failed\n"))
        return
    }

    q := strings.TrimSpace(req.FormValue("q"))
    if q == "" {
        w.WriteHeader(400)
        w.Write([]byte("empty query\n"))
        return
    }

    db := req.FormValue("db")
    if len(hs.DbList) > 0 && !hs.checkDatabase(q) && !util.MapHasKey(hs.DbMap, db) {
        w.WriteHeader(400)
        w.Write([]byte(fmt.Sprintf("database forbidden: %s\n", db)))
        return
    }

    var circle *backend.Circle
    badIds := make(map[int]bool)
    for {
        id := rand.Intn(len(hs.Circles))
        if _, ok := badIds[id]; ok {
            continue
        }
        circle = hs.Circles[id]
        if circle.IsMigrating {
            badIds[id] = true
            continue
        }
        if circle.CheckStatus() {
            break
        }
        badIds[id] = true
        if len(badIds) == len(hs.Circles) {
            w.WriteHeader(400)
            w.Write([]byte("query unavailable\n"))
            return
        }
        time.Sleep(time.Microsecond)
    }

    if !hs.CheckMeasurementQuery(q) {
        if hs.CheckClusterQuery(q) {
            var body []byte
            var err error
            if db, ok := hs.CheckCreateDatabaseQuery(q); ok {
                if len(hs.DbList) > 0 && !util.MapHasKey(hs.DbMap, db) {
                    w.WriteHeader(400)
                    w.Write([]byte(fmt.Sprintf("database forbidden: %s\n", db)))
                    return
                }
                body, err = hs.CreateDatabase(w, req)
            } else if hs.CheckDeleteOrDropQuery(q) {
                body, err = hs.DeleteOrDropMeasurement(w, req)
            } else {
                body, err = circle.QueryCluster(w, req)
            }
            if err != nil {
                log.Printf("query cluster is: %s, error: %s", q, err)
                w.WriteHeader(400)
                w.Write([]byte("query error: "+err.Error()+"\n"))
                return
            }
            w.Write(body)
            return
        }
        w.WriteHeader(400)
        w.Write([]byte("query forbidden\n"))
        return
    }

    body, err := circle.Query(w, req)
    if err != nil {
        log.Printf("query is: %s, error: %s", q, err)
        w.WriteHeader(400)
        w.Write([]byte("query error: "+err.Error()+"\n"))
        return
    }
    w.Write(body)
    return
}

func (hs *HttpService) HandlerWrite(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodPost {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    if !hs.checkAuth(req) {
        w.WriteHeader(401)
        w.Write([]byte("authentication failed\n"))
        return
    }

    precision := req.URL.Query().Get("precision")
    if precision == "" {
        precision = "ns"
    }
    db := req.URL.Query().Get("db")
    if db == "" {
        w.WriteHeader(400)
        w.Write([]byte("empty database\n"))
        return
    }
    if len(hs.DbList) > 0 && !util.MapHasKey(hs.DbMap, db) {
        w.WriteHeader(400)
        w.Write([]byte(fmt.Sprintf("database forbidden: %s\n", db)))
        return
    }

    body := req.Body
    if req.Header.Get("Content-Encoding") == "gzip" {
        b, err := gzip.NewReader(body)
        defer b.Close()
        if err != nil {
            w.Write([]byte("unable to decode gzip body\n"))
            return
        }
        body = b
    }
    p, err := ioutil.ReadAll(body)
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }

    lines := bytes.Split(p, []byte("\n"))
    for _, line := range lines {
        if len(line) == 0 {
            continue
        }
        data := &backend.LineData{
            Db:        db,
            Line:      line,
            Precision: precision,
        }
        hs.WriteData(data)
    }
    w.WriteHeader(204)
    return
}

func (hs *HttpService) HandlerHealth(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodGet {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    hs.AddJsonHeader(w)
    data := make([]map[string]interface{}, len(hs.Circles))
    for i, c := range hs.Circles {
        data[i] = map[string]interface{}{"circle": c.Name, "backends": c.GetHealth()}
    }
    res, _ := json.Marshal(data)
    res = append(res, '\n')
    w.Write(res)
    return
}

func (hs *HttpService) HandlerReplica(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodGet {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    db := req.FormValue("db")
    meas := req.FormValue("meas")
    if db != "" && meas != "" {
        hs.AddJsonHeader(w)
        key := backend.GetKey(db, meas)
        backends := hs.GetBackends(key)
        data := make([]map[string]string, len(backends))
        for i, b := range backends {
            data[i] = map[string]string{"circle": hs.Circles[i].Name, "name": b.Name, "url": b.Url}
        }
        res, _ := json.Marshal(data)
        res = append(res, '\n')
        w.Write(res)
    } else {
        w.WriteHeader(400)
        w.Write([]byte("invalid db or meas\n"))
    }
    return
}

func (hs *HttpService)HandlerEncrypt(w http.ResponseWriter, req *http.Request)  {
    defer req.Body.Close()
    if req.Method != http.MethodGet {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }
    ctx := req.URL.Query().Get("ctx")
    encrypt := util.AesEncrypt(ctx, config.CipherKey)
    w.WriteHeader(200)
    w.Write([]byte(encrypt+"\n"))
}

func (hs *HttpService)HandlerDencrypt(w http.ResponseWriter, req *http.Request)  {
    defer req.Body.Close()
    if req.Method != http.MethodGet {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }
    key := req.URL.Query().Get("key")
    ctx := req.URL.Query().Get("ctx")
    decrypt := util.AesDecrypt(ctx, key)
    w.WriteHeader(200)
    w.Write([]byte(decrypt+"\n"))
}

func (hs *HttpService) HandlerMigrating(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method == http.MethodGet {
        hs.AddJsonHeader(w)
        data := make([]map[string]interface{}, len(hs.Circles))
        for k, circle := range hs.Circles {
            data[k] = map[string]interface{}{
                "circle_id": circle.CircleId,
                "is_migrating": circle.IsMigrating,
            }
        }
        res, _ := json.Marshal(data)
        res = append(res, '\n')
        w.Write(res)
        return
    } else if req.Method == http.MethodPost {
        circleId, err := hs.formCircleId(req, "circle_id")
        if err != nil {
            w.WriteHeader(400)
            w.Write([]byte(err.Error()+"\n"))
            return
        }
        migrating, err := hs.formBool(req, "migrating")
        if err != nil {
            w.WriteHeader(400)
            w.Write([]byte("illegal migrating\n"))
            return
        }

        hs.AddJsonHeader(w)
        circle := hs.Circles[circleId]
        circle.SetMigrating(migrating)
        data := map[string]interface{}{
            "circle_id": circle.CircleId,
            "is_migrating": circle.IsMigrating,
        }
        res, _ := json.Marshal(data)
        res = append(res, '\n')
        w.Write(res)
        return
    } else {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }
}

func (hs *HttpService) HandlerRebalance(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodPost {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    circleId, err := hs.formCircleId(req, "circle_id")
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }
    operation := req.FormValue("operation")
    if operation != "add" && operation != "rm" {
        w.WriteHeader(400)
        w.Write([]byte("invalid operation\n"))
        return
    }

    var backends []*backend.Backend
    if operation == "rm" {
        var body struct {
            Backends []*backend.Backend `json:"backends"`
        }
        decoder := json.NewDecoder(req.Body)
        err := decoder.Decode(&body)
        if err != nil {
            w.WriteHeader(400)
            w.Write([]byte("invalid backends from body\n"))
            return
        }
        for _, b := range body.Backends {
            backends = append(backends, &backend.Backend{
                Name: b.Name,
                Url: b.Url,
                Username: b.Username,
                Password: b.Password,
                AuthSecure: hs.AuthSecure,
                Transport: backend.NewTransport(b.Url),
                Active: true,
            })
            hs.MigrateStats[circleId][b.Url] = &backend.MigrateInfo{}
            hs.Circles[circleId].BackendWgMap[b.Url] = &sync.WaitGroup{}
        }
    }
    for _, backend := range hs.Circles[circleId].Backends {
        backends = append(backends, backend)
    }

    err = hs.setCpus(req)
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }

    if hs.Circles[circleId].IsMigrating {
        w.WriteHeader(202)
        w.Write([]byte(fmt.Sprintf("circle %d is migrating\n", circleId)))
        return
    }
    if hs.IsResyncing {
        w.WriteHeader(202)
        w.Write([]byte("proxy is resyncing\n"))
        return
    }

    dbs := hs.formValues(req, "db")
    go hs.Rebalance(circleId, backends, dbs)
    w.WriteHeader(202)
    w.Write([]byte("accepted\n"))
    return
}

func (hs *HttpService) HandlerRecovery(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodPost {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    fromCircleId, err := hs.formCircleId(req, "from_circle_id")
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }
    toCircleId, err := hs.formCircleId(req, "to_circle_id")
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }
    if fromCircleId == toCircleId {
        w.WriteHeader(400)
        w.Write([]byte("from_circle_id and to_circle_id cannot be same\n"))
        return
    }

    err = hs.setCpus(req)
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }

    if hs.Circles[fromCircleId].IsMigrating || hs.Circles[toCircleId].IsMigrating {
        w.WriteHeader(202)
        w.Write([]byte(fmt.Sprintf("circle %d or %d is migrating\n", fromCircleId, toCircleId)))
        return
    }
    if hs.IsResyncing {
        w.WriteHeader(202)
        w.Write([]byte("proxy is resyncing\n"))
        return
    }

    backendUrls := hs.formValues(req, "backend_urls")
    dbs := hs.formValues(req, "db")
    go hs.Recovery(fromCircleId, toCircleId, backendUrls, dbs)
    w.WriteHeader(202)
    w.Write([]byte("accepted\n"))
    return
}

func (hs *HttpService) HandlerResync(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodPost {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    seconds, err := hs.formSeconds(req)
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }

    err = hs.setCpus(req)
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }

    for _, circle := range hs.Circles {
        if circle.IsMigrating {
            w.WriteHeader(202)
            w.Write([]byte(fmt.Sprintf("circle %d is migrating\n", circle.CircleId)))
            return
        }
    }
    if hs.IsResyncing {
        w.WriteHeader(202)
        w.Write([]byte("proxy is resyncing\n"))
        return
    }

    dbs := hs.formValues(req, "db")
    go hs.Resync(dbs, seconds)
    w.WriteHeader(202)
    w.Write([]byte("accepted\n"))
    return
}

func (hs *HttpService) HandlerClear(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodPost {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    circleId, err := hs.formCircleId(req, "circle_id")
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }

    err = hs.setCpus(req)
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }

    if hs.Circles[circleId].IsMigrating {
        w.WriteHeader(202)
        w.Write([]byte(fmt.Sprintf("circle %d is migrating\n", circleId)))
        return
    }
    if hs.IsResyncing {
        w.WriteHeader(202)
        w.Write([]byte("proxy is resyncing\n"))
        return
    }

    go hs.Clear(circleId)
    w.WriteHeader(202)
    w.Write([]byte("accepted\n"))
    return
}

func (hs *HttpService) HandlerStats(w http.ResponseWriter, req *http.Request) {
    defer req.Body.Close()
    hs.AddHeader(w)

    if req.Method != http.MethodGet {
        w.WriteHeader(405)
        w.Write([]byte("method not allow\n"))
        return
    }

    circleId, err := hs.formCircleId(req, "circle_id")
    if err != nil {
        w.WriteHeader(400)
        w.Write([]byte(err.Error()+"\n"))
        return
    }

    statsType := req.FormValue("type")
    if statsType == "rebalance" || statsType == "recovery" || statsType == "resync" || statsType == "clear" {
        hs.AddJsonHeader(w)
        res, _ := json.Marshal(hs.MigrateStats[circleId])
        res = append(res, '\n')
        w.Write(res)
    } else {
        w.WriteHeader(400)
        w.Write([]byte("invalid stats type\n"))
    }
    return
}

func (hs *HttpService) AddHeader(w http.ResponseWriter) {
    w.Header().Add("X-Influxdb-Version", config.Version)
}

func (hs *HttpService) AddJsonHeader(w http.ResponseWriter) {
    w.Header().Add("Content-Type", "application/json")
}

func (hs *HttpService) transAuth(ctx string) string {
    if hs.AuthSecure {
        return util.AesEncrypt(ctx, config.CipherKey)
    } else {
        return ctx
    }
}

func (hs *HttpService) checkAuth(r *http.Request) bool {
    if hs.Username == "" && hs.Password == "" {
        return true
    }
    u, p := r.URL.Query().Get("u"), r.URL.Query().Get("p")
    if hs.transAuth(u) == hs.Username && hs.transAuth(p) == hs.Password  {
        return true
    }
    u, p, ok := r.BasicAuth()
    if ok && hs.transAuth(u) == hs.Username && hs.transAuth(p) == hs.Password {
        return true
    }
    return false
}

func (hs *HttpService) checkDatabase(q string) bool {
    q = strings.ToLower(q)
    return (strings.HasPrefix(q, "show") && strings.Contains(q, "databases")) || (strings.HasPrefix(q, "create") && strings.Contains(q, "database"))
}

func (hs *HttpService) formValues(req *http.Request, key string) []string {
    var values []string
    str := strings.Trim(req.FormValue(key), ", ")
    if str != "" {
        values = strings.Split(str, ",")
    }
    return values
}

func (hs *HttpService) formPositiveInt(req *http.Request, key string) (int, bool) {
    str := strings.TrimSpace(req.FormValue(key))
    if str == "" {
        return 0, true
    }
    value, err := strconv.Atoi(str)
    return value, err == nil && value >= 0
}

func (hs *HttpService) formSeconds(req *http.Request) (int, error) {
    days, ok1 := hs.formPositiveInt(req, "days")
    hours, ok2 := hs.formPositiveInt(req, "hours")
    minutes, ok3 := hs.formPositiveInt(req, "minutes")
    seconds, ok4 := hs.formPositiveInt(req, "seconds")
    if !ok1 || !ok2 || !ok3 || !ok4 {
        return 0, errors.New("invalid days, hours, minutes or seconds")
    }
    return days * 86400 + hours * 3600 + minutes * 60 + seconds, nil
}

func (hs *HttpService) formCircleId(req *http.Request, key string) (int, error) {
    circleId, err := strconv.Atoi(req.FormValue(key))
    if err != nil || circleId < 0 || circleId >= len(hs.Circles) {
        return circleId, errors.New("invalid " + key)
    }
    return circleId, nil
}

func (hs *HttpService) formBool(req *http.Request, key string) (bool, error) {
    return strconv.ParseBool(req.FormValue(key))
}

func (hs *HttpService) setCpus(req *http.Request) error {
    str := strings.TrimSpace(req.FormValue("cpus"))
    if str != "" {
        cpus, err := strconv.Atoi(str)
        if err != nil || cpus <= 0 || cpus > runtime.NumCPU() {
            return errors.New("invalid cpus")
        }
        hs.MigrateCpus = cpus
    } else {
        hs.MigrateCpus = 1
    }
    return nil
}
