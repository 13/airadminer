package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	cookieName  = "airadminer_sid"
	idleTimeout = 12 * time.Hour
)

// Session holds one signed-in connection target. Credentials live only in memory.
type Session struct {
	mu       sync.Mutex
	id       string
	baseCfg  *pgxpool.Config // as given at login; database may be swapped
	pool     *pgxpool.Pool
	lastUsed time.Time
	fixed    bool

	metaMu    sync.Mutex
	metaCache map[string]*TableMeta // "schema.name" -> meta
	typeNames map[uint32]string
}

type App struct {
	cfg      config
	mu       sync.Mutex
	sessions map[string]*Session
	fixed    *Session
}

func newApp(cfg config) *App {
	a := &App{cfg: cfg, sessions: map[string]*Session{}}
	go a.reaper()
	return a
}

func (a *App) reaper() {
	for range time.Tick(5 * time.Minute) {
		a.mu.Lock()
		for id, s := range a.sessions {
			s.mu.Lock()
			idle := time.Since(s.lastUsed) > idleTimeout
			s.mu.Unlock()
			if idle {
				s.close()
				delete(a.sessions, id)
			}
		}
		a.mu.Unlock()
	}
}

func (a *App) closeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		s.close()
	}
	if a.fixed != nil {
		a.fixed.close()
	}
}

func (a *App) initFixed(ctx context.Context) error {
	cfg, err := pgxpool.ParseConfig(a.cfg.dsn)
	if err != nil {
		return err
	}
	s, err := a.openSession(ctx, cfg)
	if err != nil {
		return err
	}
	s.fixed = true
	a.fixed = s
	return nil
}

func (a *App) openSession(ctx context.Context, cfg *pgxpool.Config) (*Session, error) {
	s := &Session{baseCfg: cfg, lastUsed: time.Now()}
	if err := s.connect(ctx, cfg.ConnConfig.Database, a.cfg.readonly); err != nil {
		return nil, err
	}
	return s, nil
}

// connect replaces the session pool with one pointed at database db.
func (s *Session) connect(ctx context.Context, db string, readonly bool) error {
	cfg := s.baseCfg.Copy()
	cfg.ConnConfig.Database = db
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 10 * time.Minute
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "airadminer"
	if readonly {
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(cctx, cfg)
	if err != nil {
		return err
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return err
	}
	s.mu.Lock()
	old := s.pool
	s.pool = pool
	s.mu.Unlock()
	s.metaMu.Lock()
	s.metaCache = map[string]*TableMeta{}
	s.typeNames = map[uint32]string{}
	s.metaMu.Unlock()
	if old != nil {
		go old.Close()
	}
	return nil
}

func (s *Session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pool != nil {
		s.pool.Close()
		s.pool = nil
	}
}

func (s *Session) Pool() *pgxpool.Pool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
	return s.pool
}

func (s *Session) forgetMeta() {
	s.metaMu.Lock()
	s.metaCache = map[string]*TableMeta{}
	s.metaMu.Unlock()
}

func newID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *App) sessionFor(r *http.Request) *Session {
	if a.fixed != nil {
		a.fixed.mu.Lock()
		a.fixed.lastUsed = time.Now()
		a.fixed.mu.Unlock()
		return a.fixed
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[c.Value]
}

func (a *App) addSession(w http.ResponseWriter, r *http.Request, s *Session) {
	s.id = newID()
	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.id,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

func (a *App) dropSession(w http.ResponseWriter, s *Session) {
	a.mu.Lock()
	delete(a.sessions, s.id)
	a.mu.Unlock()
	s.close()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

type loginReq struct {
	URL      string `json:"url"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	SSLMode  string `json:"sslmode"`
}

func (l loginReq) dsn() (string, error) {
	if strings.TrimSpace(l.URL) != "" {
		return strings.TrimSpace(l.URL), nil
	}
	host := strings.TrimSpace(l.Host)
	if host == "" {
		host = "localhost"
	}
	port := strings.TrimSpace(l.Port)
	if port == "" {
		port = "5432"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("port must be a number")
	}
	u := url.URL{Scheme: "postgres", Path: "/" + l.Database}
	if strings.HasPrefix(host, "/") { // unix socket directory
		u.Host = ""
	} else {
		u.Host = host + ":" + port
	}
	if l.Password != "" {
		u.User = url.UserPassword(l.User, l.Password)
	} else if l.User != "" {
		u.User = url.User(l.User)
	}
	q := url.Values{}
	if strings.HasPrefix(host, "/") {
		q.Set("host", host)
		q.Set("port", port)
	}
	if l.SSLMode != "" {
		q.Set("sslmode", l.SSLMode)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
