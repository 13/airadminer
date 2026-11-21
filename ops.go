package main

// Operations: structured logs, request ids, the audit trail, Prometheus
// metrics and trusted reverse proxies.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- logging ----------

func newLogger(format string, w io.Writer) (*slog.Logger, error) {
	switch format {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, nil)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, nil)), nil
	}
	return nil, fmt.Errorf("unknown log format %q (want text or json)", format)
}

// openAuditLog returns the logger audit events go to: a JSON file of their
// own when path is set, otherwise the main log.
func openAuditLog(path string, main *slog.Logger) (*slog.Logger, io.Closer, error) {
	if path == "" {
		return main, nil, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewJSONHandler(f, nil)), f, nil
}

// ---------- trusted proxies ----------

type trustList struct{ nets []netip.Prefix }

// parseTrust reads a comma-separated list of IPs and CIDRs.
func parseTrust(spec string) (*trustList, error) {
	t := &trustList{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			t.nets = append(t.nets, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("-trust-proxy: %q is not an IP address or CIDR", part)
		}
		t.nets = append(t.nets, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
	}
	return t, nil
}

func (t *trustList) has(addr string) bool {
	if t == nil {
		return false
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, n := range t.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// viaProxy reports whether the request came straight from a trusted proxy,
// whose forwarding headers may then be believed.
func (a *App) viaProxy(r *http.Request) bool {
	return a.trust.has(hostOnly(r.RemoteAddr))
}

// clientIP is the address of the browser: the peer, or, behind trusted
// proxies, the last X-Forwarded-For entry that is not one of them.
func (a *App) clientIP(r *http.Request) string {
	peer := hostOnly(r.RemoteAddr)
	if !a.trust.has(peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if _, err := netip.ParseAddr(hops[i]); err != nil {
			break // garbage: stop at what can be trusted
		}
		if !a.trust.has(hops[i]) {
			return hops[i]
		}
	}
	return peer
}

// isSecure reports whether the browser reached us over HTTPS, directly or
// through a trusted proxy.
func (a *App) isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !a.viaProxy(r) {
		return false
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// ---------- per-request information ----------

type reqInfo struct {
	id    string
	ip    string
	actor string // the person: proxy-authenticated user or basic-auth user
	err   string // failure, for the request log
	// auditErr marks a request that answered 200 but failed, such as a SQL
	// editor run whose statement raised an error.
	auditErr error
	notes    []any // audit attributes added by the handler
}

type infoKey struct{}

func infoOf(r *http.Request) *reqInfo {
	if ri, ok := r.Context().Value(infoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{}
}

// note adds attributes to the request's audit event.
func note(r *http.Request, kv ...any) {
	ri := infoOf(r)
	ri.notes = append(ri.notes, kv...)
}

func requestID(r *http.Request, trusted bool) string {
	if trusted {
		if v := r.Header.Get("X-Request-Id"); v != "" && len(v) <= 64 && strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == "" {
			return v
		}
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ---------- audit ----------

// audit writes one audit event. Values of cells never appear in it; SQL
// text and record keys only with -audit-detail.
func (a *App) audit(r *http.Request, event string, err error, kv ...any) {
	ri := infoOf(r)
	attrs := []any{"event", event, "ok", err == nil, "request_id", ri.id, "ip", ri.ip}
	if ri.actor != "" {
		attrs = append(attrs, "actor", ri.actor)
	}
	attrs = append(attrs, kv...)
	attrs = append(attrs, ri.notes...)
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	a.auditLog.Info("audit", attrs...)
	a.metrics.count("audit", event)
}

// sqlNote describes user-written SQL for the audit log: the text itself
// with -audit-detail, otherwise its size and a fingerprint.
func (a *App) sqlNote(sql string) []any {
	if a.cfg.auditDetail {
		return []any{"sql", sql}
	}
	h := sha256.Sum256([]byte(sql))
	return []any{"sql_bytes", len(sql), "sql_sha256", hex.EncodeToString(h[:8])}
}

// ---------- metrics ----------

var durBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type histogram struct {
	counts []uint64 // per bucket, not cumulative
	sum    float64
	n      uint64
}

type metrics struct {
	mu       sync.Mutex
	requests map[[2]string]uint64 // route, status code
	duration map[string]*histogram
	counters map[[2]string]uint64 // name, label value
}

func newMetrics() *metrics {
	return &metrics{requests: map[[2]string]uint64{}, duration: map[string]*histogram{}, counters: map[[2]string]uint64{}}
}

func (m *metrics) count(name, label string) {
	m.mu.Lock()
	m.counters[[2]string{name, label}]++
	m.mu.Unlock()
}

func (m *metrics) observe(route string, code int, d time.Duration) {
	s := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[2]string{route, fmt.Sprint(code)}]++
	h := m.duration[route]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(durBuckets))}
		m.duration[route] = h
	}
	h.sum += s
	h.n++
	for i, b := range durBuckets {
		if s <= b {
			h.counts[i]++
			break
		}
	}
}

func sortedKeys[K comparable, V any](m map[K]V, less func(a, b K) bool) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return less(keys[i], keys[j]) })
	return keys
}

func pairLess(a, b [2]string) bool { return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1]) }

// handleMetrics serves the Prometheus text format.
func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	m := a.metrics
	m.mu.Lock()
	p("# HELP airadminer_http_requests_total HTTP requests by route and status.")
	p("# TYPE airadminer_http_requests_total counter")
	for _, k := range sortedKeys(m.requests, pairLess) {
		p(`airadminer_http_requests_total{route=%q,code=%q} %d`, k[0], k[1], m.requests[k])
	}
	p("# HELP airadminer_http_request_duration_seconds HTTP request duration by route.")
	p("# TYPE airadminer_http_request_duration_seconds histogram")
	for _, route := range sortedKeys(m.duration, func(a, b string) bool { return a < b }) {
		h := m.duration[route]
		var cum uint64
		for i, le := range durBuckets {
			cum += h.counts[i]
			p(`airadminer_http_request_duration_seconds_bucket{route=%q,le="%g"} %d`, route, le, cum)
		}
		p(`airadminer_http_request_duration_seconds_bucket{route=%q,le="+Inf"} %d`, route, h.n)
		p(`airadminer_http_request_duration_seconds_sum{route=%q} %g`, route, h.sum)
		p(`airadminer_http_request_duration_seconds_count{route=%q} %d`, route, h.n)
	}
	help := map[string]string{
		"logins": "Sign-in attempts by result (ok, failed, limited, full).",
		"audit":  "Audited actions by event.",
	}
	label := map[string]string{"logins": "result", "audit": "event"}
	last := ""
	for _, k := range sortedKeys(m.counters, pairLess) {
		if k[0] != last {
			last = k[0]
			p("# HELP airadminer_%s_total %s", k[0], help[k[0]])
			p("# TYPE airadminer_%s_total counter", k[0])
		}
		p(`airadminer_%s_total{%s=%q} %d`, k[0], label[k[0]], k[1], m.counters[k])
	}
	m.mu.Unlock()

	sessions, pools := a.poolStats()
	p("# HELP airadminer_sessions Signed-in browser sessions.")
	p("# TYPE airadminer_sessions gauge")
	p("airadminer_sessions %d", sessions)
	p("# HELP airadminer_db_connections Database connections over all sessions, by state.")
	p("# TYPE airadminer_db_connections gauge")
	p(`airadminer_db_connections{state="acquired"} %d`, pools[0])
	p(`airadminer_db_connections{state="idle"} %d`, pools[1])
	p(`airadminer_db_connections{state="max"} %d`, pools[2])
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p("# HELP airadminer_go_goroutines Goroutines.")
	p("# TYPE airadminer_go_goroutines gauge")
	p("airadminer_go_goroutines %d", runtime.NumGoroutine())
	p("# HELP airadminer_go_heap_bytes Heap memory in use.")
	p("# TYPE airadminer_go_heap_bytes gauge")
	p("airadminer_go_heap_bytes %d", ms.HeapAlloc)
	p("# HELP airadminer_build_info Version of this binary.")
	p("# TYPE airadminer_build_info gauge")
	p(`airadminer_build_info{version=%q} 1`, version)
	_, _ = io.WriteString(w, b.String())
}

// poolStats sums connection pool statistics over every session.
func (a *App) poolStats() (sessions int, conns [3]int64) {
	a.mu.Lock()
	all := make([]*Session, 0, len(a.sessions)+1)
	for _, s := range a.sessions {
		all = append(all, s)
	}
	if a.fixed != nil {
		all = append(all, a.fixed)
	}
	sessions = len(a.sessions)
	a.mu.Unlock()
	for _, s := range all {
		s.mu.Lock()
		db := s.db
		s.mu.Unlock()
		if db == nil {
			continue
		}
		st := db.pool.Stat()
		conns[0] += int64(st.AcquiredConns())
		conns[1] += int64(st.IdleConns())
		conns[2] += int64(st.MaxConns())
	}
	return sessions, conns
}

// opsRoutes is served on -metrics-addr, apart from the UI.
func (a *App) opsRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", a.handleMetrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

// logRequest writes the request log line for API calls.
func (a *App) logRequest(r *http.Request, ri *reqInfo, sw *statusWriter, d time.Duration) {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return
	}
	level := slog.LevelInfo
	if sw.status >= 500 {
		level = slog.LevelError
	}
	attrs := []any{"method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", d.Milliseconds(), "bytes", sw.bytes, "ip", ri.ip, "request_id", ri.id}
	if ri.actor != "" {
		attrs = append(attrs, "actor", ri.actor)
	}
	if ri.err != "" {
		attrs = append(attrs, "error", ri.err)
	}
	a.log.Log(context.Background(), level, "request", attrs...)
}
