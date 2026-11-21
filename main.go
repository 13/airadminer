// airadminer: a spreadsheet-style PostgreSQL admin in a single binary.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

var version = "dev"

type config struct {
	addr     string
	dsn      string
	readonly bool
	auth     string // "user:pass" for HTTP basic auth, optional
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.addr, "addr", envOr("AIRADMINER_ADDR", "127.0.0.1:8080"), "listen address")
	flag.StringVar(&cfg.dsn, "dsn", envOr("DATABASE_URL", ""), "connect to this database for everyone and skip the login screen (env DATABASE_URL)")
	flag.BoolVar(&cfg.readonly, "readonly", envOr("AIRADMINER_READONLY", "") != "", "open every connection with default_transaction_read_only=on (env AIRADMINER_READONLY)")
	flag.StringVar(&cfg.auth, "auth", envOr("AIRADMINER_AUTH", ""), "require HTTP basic auth, as user:password (env AIRADMINER_AUTH)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("airadminer", version)
		return
	}
	if cfg.auth != "" && !strings.Contains(cfg.auth, ":") {
		log.Fatal("-auth must look like user:password")
	}

	app := newApp(cfg)
	if cfg.dsn != "" {
		if err := app.initFixed(context.Background()); err != nil {
			log.Fatalf("cannot connect with -dsn: %v", err)
		}
	}

	host, _, _ := net.SplitHostPort(cfg.addr)
	if !isLoopback(host) && cfg.auth == "" && cfg.dsn != "" {
		log.Printf("WARNING: listening on %s with a preconfigured -dsn and no -auth: anyone who can reach this port gets your database", cfg.addr)
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("airadminer %s listening on http://%s", version, displayAddr(ln.Addr().String()))

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	app.closeAll()
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
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
		// A loopback-only server must only answer to loopback names; this
		// blocks DNS-rebinding pages from talking to it through the browser.
		if loopbackOnly && !isLoopback(hostOnly(r.Host)) {
			http.Error(w, "airadminer only answers to localhost on this address", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if a.cfg.auth != "" {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				h.Set("WWW-Authenticate", `Basic realm="airadminer", charset="UTF-8"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
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
