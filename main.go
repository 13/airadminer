// airadminer: a spreadsheet-style PostgreSQL admin in a single binary.
package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type config struct {
	addr     string
	dsn      string
	readonly bool
	auth     string // "user:pass" for HTTP basic auth, optional

	allowHosts       string // login screen: database hosts that may be reached
	allowSocket      bool   // login screen: allow unix-socket hosts
	requireTLS       bool   // login screen: never connect without TLS
	maxSessions      int
	poolSize         int
	statementTimeout time.Duration
	lockTimeout      time.Duration

	sessionIdle     time.Duration // sign out after this long without a request
	sessionLifetime time.Duration // sign out this long after sign-in, used or not

	logFormat   string // text or json
	auditFile   string // audit events go here (JSON) instead of the main log
	auditDetail bool   // audit SQL text and record keys too
	metricsAddr string // serve /metrics here
	tlsCert     string
	tlsKey      string
	trustProxy  string // IPs/CIDRs of reverse proxies whose X-Forwarded-* headers are believed
	authHeader  string // header carrying the user name set by an authenticating proxy
	noSQL       bool   // turn off the SQL editor and raw SQL filters
	csvEscape   bool   // escape spreadsheet formulas in CSV exports
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.addr, "addr", envOr("AIRADMINER_ADDR", "127.0.0.1:8080"), "listen address")
	flag.StringVar(&cfg.dsn, "dsn", envOr("DATABASE_URL", ""), "connect to this database for everyone and skip the login screen (env DATABASE_URL)")
	flag.BoolVar(&cfg.readonly, "readonly", envOr("AIRADMINER_READONLY", "") != "", "open every connection with default_transaction_read_only=on (env AIRADMINER_READONLY)")
	flag.StringVar(&cfg.auth, "auth", envOr("AIRADMINER_AUTH", ""), "require HTTP basic auth, as user:password (env AIRADMINER_AUTH)")
	flag.StringVar(&cfg.allowHosts, "allow-hosts", envOr("AIRADMINER_ALLOW_HOSTS", ""), "login screen: comma-separated database hosts, host:port pairs, IPs or CIDRs that may be reached (default: any host except link-local)")
	flag.BoolVar(&cfg.allowSocket, "allow-socket", envOr("AIRADMINER_ALLOW_SOCKET", "") != "", "login screen: allow unix-socket hosts (peer authentication then runs as this server's OS user)")
	flag.BoolVar(&cfg.requireTLS, "require-tls", envOr("AIRADMINER_REQUIRE_TLS", "") != "", "login screen: upgrade sslmode disable/allow/prefer to require")
	flag.IntVar(&cfg.maxSessions, "max-sessions", envInt("AIRADMINER_MAX_SESSIONS", 64), "most signed-in browser sessions at once")
	flag.IntVar(&cfg.poolSize, "pool-size", envInt("AIRADMINER_POOL_SIZE", 4), "database connections per session (with -dsn: shared by everyone)")
	flag.DurationVar(&cfg.statementTimeout, "statement-timeout", envDur("AIRADMINER_STATEMENT_TIMEOUT", 0), "default statement_timeout for every connection (0 = server default)")
	flag.DurationVar(&cfg.lockTimeout, "lock-timeout", envDur("AIRADMINER_LOCK_TIMEOUT", 30*time.Second), "default lock_timeout for every connection (0 = server default)")
	flag.DurationVar(&cfg.sessionIdle, "session-idle", envDur("AIRADMINER_SESSION_IDLE", 12*time.Hour), "sign a browser out after this long without a request")
	flag.DurationVar(&cfg.sessionLifetime, "session-lifetime", envDur("AIRADMINER_SESSION_LIFETIME", 24*time.Hour), "sign a browser out this long after it signed in, even while in use")
	flag.StringVar(&cfg.logFormat, "log-format", envOr("AIRADMINER_LOG_FORMAT", "text"), "log format: text or json")
	flag.StringVar(&cfg.auditFile, "audit-log", envOr("AIRADMINER_AUDIT_LOG", ""), "write audit events as JSON lines to this file (default: the main log)")
	flag.BoolVar(&cfg.auditDetail, "audit-detail", envOr("AIRADMINER_AUDIT_DETAIL", "") != "", "include SQL text and record keys in audit events (they may hold personal data)")
	flag.StringVar(&cfg.metricsAddr, "metrics-addr", envOr("AIRADMINER_METRICS_ADDR", ""), "serve Prometheus /metrics and /healthz on this address, e.g. 127.0.0.1:9090")
	flag.StringVar(&cfg.tlsCert, "tls-cert", envOr("AIRADMINER_TLS_CERT", ""), "serve HTTPS with this certificate file (PEM)")
	flag.StringVar(&cfg.tlsKey, "tls-key", envOr("AIRADMINER_TLS_KEY", ""), "private key file (PEM) for -tls-cert")
	flag.StringVar(&cfg.trustProxy, "trust-proxy", envOr("AIRADMINER_TRUST_PROXY", ""), "comma-separated IPs or CIDRs of reverse proxies whose X-Forwarded-For/-Proto and -auth-header are believed")
	flag.StringVar(&cfg.authHeader, "auth-header", envOr("AIRADMINER_AUTH_HEADER", ""), "require this header, set by an authenticating proxy from -trust-proxy, as the user's name (e.g. X-Forwarded-User)")
	flag.BoolVar(&cfg.noSQL, "no-sql", envOr("AIRADMINER_NO_SQL", "") != "", "turn off the SQL editor and raw SQL filters")
	flag.BoolVar(&cfg.csvEscape, "csv-escape-formulas", envOr("AIRADMINER_CSV_ESCAPE_FORMULAS", "") != "", "prefix CSV cells that a spreadsheet would run as formulas with an apostrophe")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(versionLine())
		return
	}
	logger, err := newLogger(cfg.logFormat, os.Stderr)
	if err != nil {
		fatal(err.Error())
	}
	slog.SetDefault(logger)
	if err := cfg.check(); err != nil {
		fatal(err.Error())
	}
	auditLog, auditFile, err := openAuditLog(cfg.auditFile, logger)
	if err != nil {
		fatal("cannot open -audit-log: " + err.Error())
	}
	if auditFile != nil {
		defer auditFile.Close()
	}

	app := newApp(cfg)
	app.log, app.auditLog = logger, auditLog
	if cfg.dsn != "" {
		if err := app.initFixed(context.Background()); err != nil {
			fatal("cannot connect with -dsn: " + err.Error())
		}
	}

	host, _, _ := net.SplitHostPort(cfg.addr)
	if !isLoopback(host) && cfg.auth == "" && cfg.authHeader == "" && cfg.dsn != "" {
		slog.Warn("listening with a preconfigured -dsn and no -auth or -auth-header: anyone who can reach this port gets your database", "addr", cfg.addr)
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // request bodies are small; exports stream out, so no WriteTimeout
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		fatal(err.Error())
	}
	scheme := "http"
	if cfg.tlsCert != "" {
		scheme = "https"
	}
	slog.Info("airadminer listening", "version", version, "commit", commit, "date", date, "url", scheme+"://"+displayAddr(ln.Addr().String()))

	go func() {
		var err error
		if cfg.tlsCert != "" {
			err = srv.ServeTLS(ln, cfg.tlsCert, cfg.tlsKey)
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal(err.Error())
		}
	}()
	var ops *http.Server
	if cfg.metricsAddr != "" {
		ops = &http.Server{Addr: cfg.metricsAddr, Handler: app.opsRoutes(), ReadHeaderTimeout: 10 * time.Second}
		mln, err := net.Listen("tcp", cfg.metricsAddr)
		if err != nil {
			fatal("-metrics-addr: " + err.Error())
		}
		slog.Info("metrics listening", "url", "http://"+displayAddr(mln.Addr().String())+"/metrics")
		go func() {
			if err := ops.Serve(mln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fatal(err.Error())
			}
		}()
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if ops != nil {
		_ = ops.Shutdown(ctx)
	}
	app.closeAll()
}

func fatal(msg string) {
	slog.Error(msg)
	os.Exit(1)
}

// check validates flag combinations.
func (c config) check() error {
	if c.auth != "" && !strings.Contains(c.auth, ":") {
		return errors.New("-auth must look like user:password")
	}
	if c.maxSessions < 1 || c.poolSize < 1 {
		return errors.New("-max-sessions and -pool-size must be at least 1")
	}
	if (c.tlsCert == "") != (c.tlsKey == "") {
		return errors.New("-tls-cert and -tls-key go together")
	}
	if _, err := parseTrust(c.trustProxy); err != nil {
		return err
	}
	if c.authHeader != "" && strings.TrimSpace(c.trustProxy) == "" {
		return errors.New("-auth-header needs -trust-proxy: the header is only believed from those proxies")
	}
	if c.sessionIdle <= 0 || c.sessionLifetime <= 0 {
		return errors.New("-session-idle and -session-lifetime must be positive")
	}
	if _, err := parseHostPolicy(c.allowHosts); err != nil {
		return err
	}
	return nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("ok\n"))
	})
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	a.registerAPI(mux)
	return a.middleware(mux)
}

func (a *App) middleware(next http.Handler) http.Handler {
	user, pass, _ := strings.Cut(a.cfg.auth, ":")
	bindHost, _, _ := net.SplitHostPort(a.cfg.addr)
	loopbackOnly := isLoopback(bindHost)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" { // liveness probes carry no credentials or loopback Host
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		proxied := a.viaProxy(r)
		ri := &reqInfo{id: requestID(r, proxied), ip: a.clientIP(r)}
		r = r.WithContext(context.WithValue(r.Context(), infoKey{}, ri))
		sw := &statusWriter{ResponseWriter: w}
		w = sw
		defer func() {
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			d := time.Since(start)
			route := r.Pattern // set by the mux on this request
			if route == "" {
				route = "other"
			}
			a.metrics.observe(route, sw.status, d)
			a.logRequest(r, ri, sw, d)
		}()
		h := w.Header()
		h.Set("X-Request-Id", ri.id)
		// A loopback-only server must only answer to loopback names; this
		// blocks DNS-rebinding pages from talking to it through the browser.
		// A trusted reverse proxy on this host may forward its own Host.
		if loopbackOnly && !proxied && !isLoopback(hostOnly(r.Host)) {
			http.Error(w, "airadminer only answers to localhost on this address", http.StatusMisdirectedRequest)
			return
		}
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if a.isSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if a.cfg.authHeader != "" {
			// Only the authenticating proxy may set the user header; anyone
			// reaching this port directly could forge it.
			if !proxied {
				http.Error(w, "requests must come through the authenticating proxy", http.StatusForbidden)
				return
			}
			u := strings.TrimSpace(r.Header.Get(a.cfg.authHeader))
			if u == "" {
				http.Error(w, "the proxy did not say who is signed in", http.StatusUnauthorized)
				return
			}
			ri.actor = u
		}
		if a.cfg.auth != "" {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				h.Set("WWW-Authenticate", `Basic realm="airadminer", charset="UTF-8"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			if ri.actor == "" {
				ri.actor = u
			}
		}
		next.ServeHTTP(w, r)
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}
