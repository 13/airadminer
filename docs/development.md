# Build and development

[← README](../README.md)

There is no runtime, no Node build, no config file, and no external assets. The UI is plain HTML, CSS and JavaScript, compiled into the Go binary with `go:embed`.

```sh
make build      # ./airadminer for this machine
make dist       # static linux/amd64 binary in ./dist (override with PLATFORMS="os/arch ...")
make test       # go vet, go test -race, JS syntax check
make lint       # gofmt, vet, staticcheck, govulncheck
make test-integration  # tests against PostgreSQL (needs `make demo` running)
make e2e        # browser tests of the grid in headless Chrome (needs `make demo`)
make fuzz       # run every fuzz test for FUZZTIME (default 30s) each
make soak       # SOAKTIME (default 10m) of mixed load, killing database connections every 20 s
make demo       # throwaway Postgres on :55432 with sample data (password: secret)
```

Project layout:

```
main.go      flags, HTTP server, security middleware
session.go   login sessions and connection pools
login.go     login screen: connection settings, host policy, rate limit
meta.go      catalog queries: tables, columns, keys, indexes, constraints
api.go       JSON API: rows, count, update/insert/delete, SQL, CSV export
cells.go     cutting long values, loading full values, connection limits
ops.go       logs, request ids, audit events, metrics, trusted proxies
export.go    CSV export with formula escaping
web/         the entire UI: index.html, app.css, app.js (no build step)
```
