# airadminer

A spreadsheet interface for PostgreSQL that ships as a single binary.

Deploy it the way you'd deploy Adminer: copy one file, run it, and open the browser. Use it like Airtable: scroll through millions of rows, edit cells in place, paste blocks from a spreadsheet, filter, sort, and open any row as a form.

```
./airadminer                      # then open http://127.0.0.1:8080
```

There is no runtime, no Node build, no config file, and no external assets. The UI is plain HTML, CSS and JavaScript, compiled into the Go binary with `go:embed`.

## Features

**Grid**
- Virtual scrolling. Rows load in pages as you scroll, so a 50-million-row table opens as fast as a 50-row one. Only the rows and columns on screen are drawn, and pages far from view are dropped, so memory stays flat however far you scroll. Changing a filter or sort cancels the requests of the old one.
- Long values (text, JSON, arrays, XML …) reach the grid cut to a few thousand characters and marked with "…", so a page of rows stays small. Opening a cell, the record form, copying or overwriting loads the full value first. A long value is never saved shortened.
- Each cell renders according to its data type. Numbers are right-aligned and tabular. Booleans show as checkboxes you can click. Enums get stable colored tags. Arrays become chips. JSON is syntax-tinted. Timestamps are split into date and time. Foreign keys show as link pills, and clicking one opens the referenced record.
- Spreadsheet keyboard model: arrow keys, Tab, Enter, F2, Home and End, PageUp and PageDown, and Shift to extend a selection. Start typing to replace a cell's value. Delete sets a cell to null, Space expands the row, Ctrl/⌘+Shift+D duplicates the selected rows, and Ctrl/⌘+Z undoes the last change.
- Undo covers cell edits, pastes, clears, duplicates and records added from the form. A paste or clear undoes in one step, and records it added are removed again. Deletes cannot be undone.
- Right-click a cell or row number for a menu: expand, copy, clear, duplicate, delete.
- Copy and paste use the TSV format that Sheets, Excel and Airtable use. Paste a block to fill a range, or paste one value to fill the whole selection. A block that runs past the last row adds new records; empty cells there get the column default. Each paste or clear is saved in one transaction, so it lands completely or not at all.
- Selecting several cells shows Sum, Average, Min and Max for numeric columns in the footer. Whole numbers add up exactly, beyond 2^53 too.
- The first column and the row numbers stay frozen. Columns start as wide as their values, not their names; long field names are cut in the header and shown in full on hover. You can resize columns, double-click a column edge to fit its content, and pick a short, medium or tall row height.
- The View menu also has a compact density (smaller rows, padding and type, for all tables) and "Fit columns to screen" (Ctrl/⌘+Shift+F), which scales the columns to the visible width without changing their saved widths. When even the narrowest columns cannot all fit, the grid scrolls sideways as usual.
- Filters are built per type (is, contains, <, ≥, is empty, is checked …) and can include raw SQL conditions. Sorting can use several fields, and there is a search box that matches any field.
- Hidden fields, column widths, sorts, filters, row height and fit to screen are remembered for each table in your browser.

**Records**
- The expanded record form has a type-aware editor for every field: a true/false/null toggle for booleans, tag pickers for enums, monospace editors for JSON, and an explicit "null" button. When you create a record, any field you leave untouched uses its column default.
- Duplicate a record from its form. The copy opens prefilled, so you can change unique fields such as a SKU or email first. You can also duplicate several selected rows at once. Copies are made on the server with `INSERT … SELECT`, so binary data copies too, and identity, generated and defaulted primary-key columns get fresh values.
- Links between records work both ways. Outgoing foreign keys open the referenced record in place, with a back button. Incoming foreign keys are listed as "Linked records" with a row count, and clicking one opens that table filtered to the matching rows.
- When you edit a foreign-key cell, a search box suggests rows from the referenced table.

**Everything else**
- The Structure tab lists fields, types, defaults, indexes with their sizes, constraints, references, and view definitions.
- The SQL editor keeps up to 10,000 rows or 32 MB of a result and reports how many rows the statement returned in total. Rows past the limit are read and dropped, not canceled, so earlier statements of the same script are not rolled back. It has syntax highlighting, multi-statement scripts, running only the selected text, query cancellation, per-database history, results shown in the same grid, and errors marked at the exact line and column.
- Export the filtered view of any table to CSV. The export is streamed from `COPY`, so it doesn't hit the page size limit. Query results can be exported to CSV as well.
- Ctrl/⌘+K opens a command palette that jumps to any table, switches database, or changes the theme, density or layout.
- Ctrl/⌘+B hides or shows the sidebar. The choice is remembered.
- Light and dark themes. The theme button cycles between following the system setting (the default), light and dark. The layout also works on phones.

## Install

Build once with Go 1.25 or newer, then copy the binary anywhere. It has no dependencies, not even libc:

```sh
make build          # or: make dist, for a linux/amd64 release binary
```

Docker (about 16 MB, built `FROM scratch`, linux/amd64). CI publishes images to the GitHub Container Registry: `latest` and `1.2.3` / `1.2` for release tags, `edge` for the main branch.

```sh
docker run --rm -p 8080:8080 ghcr.io/<owner>/airadminer:latest
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

## Security notes

- By default the server listens on loopback only. On a loopback address it answers only requests whose `Host` is a loopback name, which blocks DNS-rebinding attacks.
- Every API call needs a custom request header, and the session cookie is `HttpOnly` and `SameSite=Strict`. This stops other sites from driving your session (CSRF). A strict Content-Security-Policy blocks inline scripts.
- **`-dsn` without `-auth` on a non-loopback address gives your database to anyone who can reach the port.** The server prints a warning at startup in that case.
- `-readonly` is a safety rail, not a security boundary: a user with SQL access can turn it off with `SET`. For real read-only access, connect as a PostgreSQL role that only has `SELECT`.
- The login screen connects only with what the browser sent. It never fills in a missing password from the server's `PGPASSWORD` or `~/.pgpass`, and it never uses client certificates or service files from the account airadminer runs as. A connection URL may only carry host, port, user, password, database and `sslmode`. If `PGSERVICE` is set, the login screen refuses to connect.
- Unix-socket hosts are off by default. With peer authentication they would log people in as the OS user airadminer runs as.
- The login screen can reach any database host the server can reach. On a shared server, limit it with `-allow-hosts`. The check runs on every resolved address at connect time. Link-local addresses, where cloud metadata services live, are refused unless listed.
- Sign-in attempts are rate-limited per client address (a burst of 10, then one every 6 seconds), and `-max-sessions` caps open sessions and their connection pools.
- After every SQL editor run, and every grid query with a raw SQL filter, the connection is reset with `DISCARD ALL`. A `SET ROLE`, `SET session_replication_role`, temp table or advisory lock therefore never carries over to later edits. If the reset fails, the connection is closed.
- Each session has `-pool-size` connections. SQL editor queries and CSV exports may use all but one of them, so browsing and saving keep working while long queries run. A request that finds no free connection for 20 seconds fails with HTTP 503 instead of queueing forever.
- Every request names the database its page was built for. If another tab switched the session to a different database, edits are refused (HTTP 409) and the page reloads.
- For TLS, use `-tls-cert`/`-tls-key`, or a reverse proxy listed in `-trust-proxy` (see Operations). The UI uses relative URLs, so it also works under a sub-path.

## Operations

**Logs.** One line per API request: method, path, status, duration, bytes, client IP, request ID and, when known, the signed-in person. Use `-log-format json` for log shippers. Every response carries an `X-Request-Id` header. A trusted proxy can supply its own request ID.

**Audit log.** Writes are recorded whether they succeed or fail: update, insert, delete, duplicate, batch (paste, clear, undo). So are SQL editor runs, CSV exports, sign-ins (failed ones too), sign-outs and database switches. Each event names the person, client IP, request ID, database, PostgreSQL role, table, columns and number of records. Cell values are never recorded. SQL text and record keys can hold personal data, so by default a run is recorded only by its size and a SHA-256 fingerprint; `-audit-detail` records the text and keys. `-audit-log FILE` keeps audit events apart from the main log.

**Metrics.** `-metrics-addr 127.0.0.1:9090` serves Prometheus metrics on a separate listener without authentication. Keep that address internal. The metrics are:
- requests and duration histograms per route and status
- sign-in attempts by result
- audited actions
- open sessions
- database connections (acquired, idle, max)
- goroutines, heap size and the build version

**HTTPS.** Either serve it directly with `-tls-cert`/`-tls-key`, or put a reverse proxy in front and list it in `-trust-proxy`. Behind a trusted proxy, the client address comes from `X-Forwarded-For` (used for rate limits and logs), and `X-Forwarded-Proto: https` counts as HTTPS. Over HTTPS the session cookie is `Secure` with the `__Host-` prefix, and responses carry `Strict-Transport-Security`.

**Single sign-on.** Run an authenticating proxy such as oauth2-proxy (OIDC, SAML through your IdP) in front, list it in `-trust-proxy`, and set `-auth-header X-Forwarded-User`, or whichever header your proxy sets. Requests that do not come from the proxy are refused (403), so the header cannot be forged by reaching the port directly. The header's value appears in logs and audit events. With the login screen, people still sign in to PostgreSQL with their own credentials. With `-dsn`, the proxy is the only gate.

**Locking down.** `-no-sql` removes the SQL editor and raw SQL filters on the server and hides them in the UI, leaving only the grid's own parameterised queries. Combine it with `-readonly` and a PostgreSQL role with limited grants for view-only access. `-csv-escape-formulas` protects people who open exports in Excel or Sheets from formula injection. Numbers such as `-5` are left as they are.

**With `-dsn`,** all browsers share one session until one of them switches database. That browser then gets a session and pool of its own, counted against `-max-sessions`, so nobody else is moved.

## How editing works

Rows are identified by their primary key. Tables without one fall back to `ctid` together with `xmin`. That pair names one version of one row, so if the row changed, or was deleted and its slot reused by `VACUUM`, the edit is refused instead of landing on another row. Reload the rows and try again. Views without a primary key are read-only.

Values travel as PostgreSQL's own text format in both directions, the same text `psql` shows. Each edit is one `UPDATE … WHERE <key> RETURNING …`. A paste, clear or undo sends all its changes in one transaction. It is rejected if the row no longer matches exactly one record.

A cell cut in the grid comes with the md5 of its full value. Before such a cell is overwritten by a paste, a clear or an undo, the browser loads the full value and checks the hash, so the conflict check below and undo always work on the whole value.

Edits never silently overwrite someone else's work. Each edit also sends the values you saw in the cells you changed. If someone else has changed one of those cells since, the edit is refused and you are asked to refresh. Undo works the same way: it reverts a cell only if the cell still holds what your change wrote.

In the default order (by primary key, or by `ctid`), a page next to one already loaded is fetched from that page's first or last key instead of by OFFSET. Scrolling on from any point in a table of millions of rows therefore costs the same as at the top. A jump far down with the scrollbar, or a sort of your own, still uses OFFSET. The page is cut out first, and cut values and hashes are computed only for the rows sent.

If the database drops a connection (restart, failover, a killed backend), the request answers 503 so the browser can try again. Grid reads retry once on a fresh connection on their own.

Writes are addressed by record, not by position on screen. An edit still lands on its record if a refresh or a new sort moved that record, and edits to one record are saved in the order you made them. A paste or clear first loads every row it covers, so rows that were not on screen are not skipped. Copying and pasting within airadminer keeps NULL apart from empty text. JSON is re-indented for editing without touching numbers, strings or key order. The cell then shows the value the database returned, so triggers and defaults are reflected right away.

## Build

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
