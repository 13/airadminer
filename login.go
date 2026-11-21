package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type loginReq struct {
	URL      string `json:"url"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	SSLMode  string `json:"sslmode"`
}

var sslModes = map[string]bool{"": true, "disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}

// fromURL fills the request from a postgres:// URL. Only host, port, user,
// password, database and sslmode are accepted: any other parameter could
// make the server read local files (passfile, sslkey, service) or change
// how it connects, so it is rejected instead of passed through.
func (l loginReq) fromURL() (loginReq, error) {
	u, err := url.Parse(strings.TrimSpace(l.URL))
	if err != nil {
		return l, errors.New("the connection URL is not valid")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return l, errors.New("the connection URL must start with postgres://")
	}
	out := loginReq{Host: u.Hostname(), Port: u.Port(), Database: strings.TrimPrefix(u.Path, "/")}
	if strings.Contains(u.Host, ",") {
		return l, errors.New("connection URLs with several hosts are not supported")
	}
	if u.User != nil {
		out.User = u.User.Username()
		out.Password, _ = u.User.Password()
	}
	for k, v := range u.Query() {
		switch k {
		case "sslmode":
			out.SSLMode = v[len(v)-1]
		case "host": // unix socket directory, as libpq writes it
			out.Host = v[len(v)-1]
		case "port":
			out.Port = v[len(v)-1]
		default:
			return l, fmt.Errorf("the connection URL parameter %q is not supported here", k)
		}
	}
	return out, nil
}

// quoteKV quotes a value for a PostgreSQL keyword/value connection string.
func quoteKV(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

func isSocketHost(h string) bool { return strings.HasPrefix(h, "/") || strings.HasPrefix(h, "@") }

// loginConfig turns what a browser typed into a pool config. It never lets
// the server's own environment fill in the blanks: PGPASSWORD, ~/.pgpass,
// service files and client certificates of the account airadminer runs as
// must not be handed to whoever reaches the login screen.
func (a *App) loginConfig(req loginReq) (*pgxpool.Config, error) {
	if strings.TrimSpace(req.URL) != "" {
		var err error
		if req, err = req.fromURL(); err != nil {
			return nil, err
		}
	}
	host := strings.TrimSpace(req.Host)
	if host == "" {
		host = "localhost"
	}
	port := strings.TrimSpace(req.Port)
	if port == "" {
		port = "5432"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, errors.New("port must be a number between 1 and 65535")
	}
	user := strings.TrimSpace(req.User)
	if user == "" {
		return nil, errors.New("enter a user name")
	}
	if !sslModes[req.SSLMode] {
		return nil, fmt.Errorf("unknown sslmode %q", req.SSLMode)
	}
	if a.cfg.requireTLS && (req.SSLMode == "" || req.SSLMode == "disable" || req.SSLMode == "allow" || req.SSLMode == "prefer") && !isSocketHost(host) {
		req.SSLMode = "require"
	}
	if isSocketHost(host) {
		if !a.cfg.allowSocket {
			return nil, errors.New("connecting through a unix socket is disabled on this server (start it with -allow-socket)")
		}
	} else if strings.ContainsAny(host, ",/ \t") {
		return nil, errors.New("host is not valid")
	}
	if err := a.policy.checkName(host, port); err != nil {
		return nil, err
	}

	kv := []string{
		"host=" + quoteKV(host), "port=" + quoteKV(port), "user=" + quoteKV(user),
		"password=" + quoteKV(req.Password), "dbname=" + quoteKV(req.Database),
		"passfile=''", "sslcert=''", "sslkey=''", "sslpassword=''",
		"target_session_attrs=any", "connect_timeout=10",
	}
	if req.SSLMode != "" {
		kv = append(kv, "sslmode="+req.SSLMode)
	}
	if os.Getenv("PGSERVICE") != "" {
		// A service file may override settings we cannot override back.
		return nil, errors.New("this server has PGSERVICE set, which the login screen does not support; unset it or use -dsn")
	}
	cfg, err := pgxpool.ParseConfig(strings.Join(kv, " "))
	if err != nil {
		return nil, errors.New("invalid connection settings")
	}
	cc := cfg.ConnConfig
	cc.Password = req.Password // never a password from PGPASSWORD or a passfile
	if cc.Host != host || strconv.Itoa(int(cc.Port)) != port || cc.User != user || cc.Database != req.Database {
		return nil, errors.New("the server environment overrides these connection settings; refusing to connect")
	}
	for _, fb := range cc.Fallbacks {
		if fb.Host != host {
			return nil, errors.New("the server environment overrides these connection settings; refusing to connect")
		}
		if fb.TLSConfig != nil {
			fb.TLSConfig.Certificates = nil
		}
	}
	if cc.TLSConfig != nil {
		cc.TLSConfig.Certificates = nil
	}
	if !isSocketHost(host) {
		cc.LookupFunc = a.policy.lookup(host, port)
	}
	return cfg, nil
}

// hostPolicy decides which database servers the login screen may reach.
// Without -allow-hosts every host is allowed except link-local addresses
// (cloud metadata endpoints live there and no database does).
type hostPolicy struct {
	names map[string]bool // "host" or "host:port", lower case
	nets  []netip.Prefix
}

var linkLocal = []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("fe80::/10")}

func parseHostPolicy(spec string) (*hostPolicy, error) {
	p := &hostPolicy{names: map[string]bool{}}
	for _, e := range strings.Split(spec, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if pfx, err := netip.ParsePrefix(e); err == nil {
			p.nets = append(p.nets, pfx.Masked())
			continue
		}
		if ip, err := netip.ParseAddr(strings.Trim(e, "[]")); err == nil {
			p.nets = append(p.nets, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
			continue
		}
		p.names[strings.Trim(e, "[]")] = true
	}
	return p, nil
}

func (p *hostPolicy) open() bool { return len(p.names) == 0 && len(p.nets) == 0 }

func (p *hostPolicy) nameAllowed(host, port string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	return p.names[h] || p.names[net.JoinHostPort(h, port)]
}

// checkName rejects a host up front when a list is configured and the host
// is neither a listed name nor something that may resolve into a listed net.
func (p *hostPolicy) checkName(host, port string) error {
	if p.open() || isSocketHost(host) || p.nameAllowed(host, port) {
		return nil
	}
	if len(p.nets) == 0 {
		return fmt.Errorf("connecting to %s is not allowed on this server", host)
	}
	return nil // decided per resolved address in lookup
}

func (p *hostPolicy) ipAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, n := range p.nets {
		if n.Contains(ip) {
			return true
		}
	}
	if !p.open() {
		return false
	}
	for _, n := range linkLocal {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// lookup resolves the host and keeps only addresses the policy allows. It
// runs at dial time, so a name that later resolves somewhere else is
// checked again (no DNS-rebinding window between check and connect).
func (p *hostPolicy) lookup(host, port string) func(ctx context.Context, h string) ([]string, error) {
	byName := p.nameAllowed(host, port)
	return func(ctx context.Context, h string) ([]string, error) {
		addrs, err := net.DefaultResolver.LookupHost(ctx, h)
		if err != nil {
			return nil, err
		}
		if byName {
			return addrs, nil
		}
		var ok []string
		for _, a := range addrs {
			if ip, err := netip.ParseAddr(a); err == nil && p.ipAllowed(ip) {
				ok = append(ok, a)
			}
		}
		if len(ok) == 0 {
			return nil, fmt.Errorf("connecting to %s is not allowed on this server", host)
		}
		return ok, nil
	}
}

// loginLimiter allows a burst of login attempts per client address and
// then one attempt every refill interval.
type loginLimiter struct {
	mu      sync.Mutex
	burst   float64
	refill  time.Duration
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLoginLimiter(burst int, refill time.Duration) *loginLimiter {
	return &loginLimiter{burst: float64(burst), refill: refill, buckets: map[string]*bucket{}}
}

func (l *loginLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.at))/float64(l.refill))
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep forgets clients whose bucket has refilled completely.
func (l *loginLimiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.tokens+float64(now.Sub(b.at))/float64(l.refill) >= l.burst {
			delete(l.buckets, k)
		}
	}
}
