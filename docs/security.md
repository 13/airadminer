# Security notes

[← README](../README.md)

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
