# Operations

[← README](../README.md)

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
