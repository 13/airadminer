package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	cookieName = "airadminer_sid"
	// Over HTTPS the cookie gets the __Host- prefix: browsers then insist on
	// Secure, Path=/ and no Domain, so no subdomain can set or shadow it.
	hostCookieName = "__Host-airadminer_sid"
)

// DB is one connection pool pointed at one database, together with the
// catalog cache that belongs to it. Switching database swaps the whole DB,
// so a request that started on one database finishes on it, and metadata
// of one database can never be cached under another.
type DB struct {
	name string
	user string // PostgreSQL role, for the audit log
	pool *pgxpool.Pool

	metaMu    sync.Mutex
	metaCache map[string]*TableMeta // schema + "\x00" + name -> meta
	typeNames map[uint32]string
	// slowValues: columns whose exact value query timed out, by
	// table oid + "\x00" + column, with when it did.
	slowValues map[string]time.Time

	// long holds one token per running SQL-editor query or CSV export. It
	// has one slot fewer than the pool, so browsing and saving always find
	// a connection even while long queries run.
	long chan struct{}
}

// Session holds one signed-in connection target. Credentials live only in memory.
type Session struct {
	mu       sync.Mutex
	id       string
	baseCfg  *pgxpool.Config // as given at login; database may be swapped
	db       *DB
	created  time.Time
	lastUsed time.Time
	fixed    bool // connects with -dsn: no sign-in or sign-out
}

type App struct {
	cfg      config
	policy   *hostPolicy
	logins   *loginLimiter
	trust    *trustList
	log      *slog.Logger
	auditLog *slog.Logger
	metrics  *metrics
	mu       sync.Mutex
	sessions map[string]*Session
	fixed    *Session
}

func newApp(cfg config) *App {
	policy, _ := parseHostPolicy(cfg.allowHosts)
	if cfg.maxSessions <= 0 {
		cfg.maxSessions = 64
	}
	if cfg.poolSize <= 0 {
		cfg.poolSize = 4
	}
	if cfg.sessionIdle <= 0 {
		cfg.sessionIdle = 12 * time.Hour
	}
	if cfg.sessionLifetime <= 0 {
		cfg.sessionLifetime = 24 * time.Hour
	}
	trust, _ := parseTrust(cfg.trustProxy)
	a := &App{cfg: cfg, policy: policy, logins: newLoginLimiter(10, 6*time.Second), trust: trust,
		log: slog.Default(), auditLog: slog.Default(), metrics: newMetrics(), sessions: map[string]*Session{}}
	go a.reaper()
	return a
}

func (a *App) reaper() {
	for range time.Tick(5 * time.Minute) {
		var idle []*Session
		a.mu.Lock()
		for id, s := range a.sessions {
			if !a.alive(s) {
				idle = append(idle, s)
				delete(a.sessions, id)
			}
		}
		a.mu.Unlock()
		for _, s := range idle {
			go s.close()
		}
		a.logins.sweep(time.Now())
	}
}

func (a *App) closeAll() {
	a.mu.Lock()
	all := make([]*Session, 0, len(a.sessions)+1)
	for _, s := range a.sessions {
		all = append(all, s)
	}
	if a.fixed != nil {
		all = append(all, a.fixed)
	}
	a.mu.Unlock()
	for _, s := range all {
		s.close()
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

// alive reports whether a session may still be used: not idle for longer
// than -session-idle, nor older than -session-lifetime. The shared -dsn
// session never expires.
func (a *App) alive(s *Session) bool {
	if s == a.fixed {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	return now.Sub(s.lastUsed) < a.cfg.sessionIdle && now.Sub(s.created) < a.cfg.sessionLifetime
}

func (a *App) openSession(ctx context.Context, cfg *pgxpool.Config) (*Session, error) {
	now := time.Now()
	s := &Session{baseCfg: cfg, created: now, lastUsed: now}
	if err := s.connect(ctx, cfg.ConnConfig.Database, a.cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// connect replaces the session's database with a fresh pool on database db.
func (s *Session) connect(ctx context.Context, db string, ac config) error {
	cfg := s.baseCfg.Copy()
	cfg.ConnConfig.Database = db
	cfg.MaxConns = int32(ac.poolSize)
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 10 * time.Minute
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = time.Minute
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["application_name"] = "airadminer"
	if ac.readonly {
		rp["default_transaction_read_only"] = "on"
	}
	// Server-side guards, so a stuck lock or an abandoned transaction can
	// never hold a connection forever. Statements the user runs in the SQL
	// editor may raise statement_timeout with SET; that is reset afterwards.
	if ac.statementTimeout > 0 {
		rp["statement_timeout"] = durMS(ac.statementTimeout)
	}
	if ac.lockTimeout > 0 {
		rp["lock_timeout"] = durMS(ac.lockTimeout)
	}
	rp["idle_in_transaction_session_timeout"] = durMS(5 * time.Minute)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(cctx, cfg)
	if err != nil {
		return err
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return err
	}
	next := &DB{name: db, user: cfg.ConnConfig.User, pool: pool, metaCache: map[string]*TableMeta{}, typeNames: map[uint32]string{}, long: make(chan struct{}, max(1, cfg.MaxConns-1))}
	if next.name == "" {
		next.name = cfg.ConnConfig.Database
	}
	_ = pool.QueryRow(cctx, "SELECT current_database()").Scan(&next.name)
	s.mu.Lock()
	old := s.db
	s.db = next
	s.mu.Unlock()
	if old != nil {
		go old.pool.Close() // waits for requests still running on it
	}
	return nil
}

func durMS(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func (s *Session) close() {
	s.mu.Lock()
	db := s.db
	s.db = nil
	s.mu.Unlock()
	if db != nil {
		db.pool.Close()
	}
}

// current returns the database this session is on, or nil when closed.
func (s *Session) current() *DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
	return s.db
}

func (d *DB) forgetMeta() {
	d.metaMu.Lock()
	d.metaCache = map[string]*TableMeta{}
	d.metaMu.Unlock()
}

func newID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func sessionID(r *http.Request) string {
	for _, name := range []string{hostCookieName, cookieName} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// sessionFor returns the browser's session. With -dsn every browser shares
// one session until it switches database, which gives it its own.
func (a *App) sessionFor(r *http.Request) *Session {
	if id := sessionID(r); id != "" {
		a.mu.Lock()
		s := a.sessions[id]
		a.mu.Unlock()
		if s != nil && a.alive(s) {
			return s
		}
		if s != nil {
			a.mu.Lock()
			delete(a.sessions, id)
			a.mu.Unlock()
			go s.close()
		}
	}
	return a.fixed
}

var errTooManySessions = errors.New("too many people are signed in to this server; try again later")

func (a *App) sessionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions)
}

func (a *App) addSession(w http.ResponseWriter, r *http.Request, s *Session) error {
	s.id = newID()
	a.mu.Lock()
	if len(a.sessions) >= a.cfg.maxSessions {
		a.mu.Unlock()
		return errTooManySessions
	}
	a.sessions[s.id] = s
	a.mu.Unlock()
	name, secure := cookieName, a.isSecure(r)
	if secure {
		name = hostCookieName
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    s.id,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (a *App) dropSession(w http.ResponseWriter, s *Session) {
	a.mu.Lock()
	delete(a.sessions, s.id)
	a.mu.Unlock()
	go s.close()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: hostCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}
