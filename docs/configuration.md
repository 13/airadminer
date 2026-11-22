# Install and configuration

[← README](../README.md)

## Install

Build once with Go 1.25 or newer, then copy the binary anywhere. It has no dependencies, not even libc:

```sh
make build          # or: make dist, for a linux/amd64 release binary
```

Docker (about 16 MB, built `FROM scratch`, linux/amd64). CI publishes images to the GitHub Container Registry: `latest` and `1.2.3` / `1.2` for release tags, `edge` for the main branch.

```sh
docker run --rm -p 8080:8080 ghcr.io/13/airadminer:latest
# or build it yourself
make docker            # tags airadminer:<version> and airadminer:latest
docker run --rm -p 8080:8080 airadminer
```

## Run

```sh
./airadminer                                  # login screen at http://127.0.0.1:8080
./airadminer -addr 0.0.0.0:9000 -auth admin:s3cret
DATABASE_URL=postgres://app@db/app ./airadminer -readonly
```

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-addr` | `AIRADMINER_ADDR` | `127.0.0.1:8080` | Listen address |
| `-dsn` | `DATABASE_URL` | – | Connect everyone to this database and skip the login screen |
| `-auth` | `AIRADMINER_AUTH` | – | Require HTTP basic auth, as `user:password` |
| `-readonly` | `AIRADMINER_READONLY` | off | Open every connection with `default_transaction_read_only=on` and disable editing |
| `-allow-hosts` | `AIRADMINER_ALLOW_HOSTS` | any host except link-local | Login screen: comma-separated hosts, `host:port` pairs, IPs or CIDRs it may connect to |
| `-allow-socket` | `AIRADMINER_ALLOW_SOCKET` | off | Login screen: allow unix-socket hosts |
| `-require-tls` | `AIRADMINER_REQUIRE_TLS` | off | Login screen: upgrade `sslmode` `disable`/`allow`/`prefer` to `require` |
| `-max-sessions` | `AIRADMINER_MAX_SESSIONS` | `64` | Most signed-in browser sessions at once |
| `-pool-size` | `AIRADMINER_POOL_SIZE` | `4` | Database connections per session (with `-dsn`, the shared session and each browser that switched database) |
| `-statement-timeout` | `AIRADMINER_STATEMENT_TIMEOUT` | server default | Default `statement_timeout` for every connection, e.g. `5m` |
| `-lock-timeout` | `AIRADMINER_LOCK_TIMEOUT` | `30s` | Default `lock_timeout` for every connection |
| `-session-idle` | `AIRADMINER_SESSION_IDLE` | `12h` | Sign a browser out after this long without a request |
| `-session-lifetime` | `AIRADMINER_SESSION_LIFETIME` | `24h` | Sign a browser out this long after it signed in, even while in use |
| `-tls-cert`, `-tls-key` | `AIRADMINER_TLS_CERT`, `AIRADMINER_TLS_KEY` | – | Serve HTTPS (TLS 1.2+) with these PEM files |
| `-trust-proxy` | `AIRADMINER_TRUST_PROXY` | – | IPs or CIDRs of reverse proxies whose `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Request-Id` and `-auth-header` are believed |
| `-auth-header` | `AIRADMINER_AUTH_HEADER` | – | Sign-in through an authenticating proxy: require this header (e.g. `X-Forwarded-User`) from a `-trust-proxy` address |
| `-no-sql` | `AIRADMINER_NO_SQL` | off | Turn off the SQL editor and raw SQL filters |
| `-csv-escape-formulas` | `AIRADMINER_CSV_ESCAPE_FORMULAS` | off | Prefix CSV cells a spreadsheet would run as formulas with `'` |
| `-log-format` | `AIRADMINER_LOG_FORMAT` | `text` | `text` or `json` |
| `-audit-log` | `AIRADMINER_AUDIT_LOG` | main log | Write audit events as JSON lines to this file (mode 0600) |
| `-audit-detail` | `AIRADMINER_AUDIT_DETAIL` | off | Include SQL text and record keys in audit events |
| `-metrics-addr` | `AIRADMINER_METRICS_ADDR` | – | Serve Prometheus `/metrics` and `/healthz` on this address |

`GET /healthz` answers `ok` without authentication, for liveness probes.

Without `-dsn`, you see a login screen like Adminer's. Credentials are held in the server's memory for the session, which ends after `-session-idle` without use or `-session-lifetime` after sign-in, and are never written to disk. Each browser session gets its own small connection pool.
