# How editing works

[← README](../README.md)

Rows are identified by their primary key. Tables without one fall back to `ctid` together with `xmin`. That pair names one version of one row, so if the row changed, or was deleted and its slot reused by `VACUUM`, the edit is refused instead of landing on another row. Reload the rows and try again. Views without a primary key are read-only. In a table that other tables inherit from, such as a TimescaleDB hypertable, the same `ctid` and `xmin` can appear in every child, so the key also carries the row's `tableoid`.

TimescaleDB: hypertables show their real size and row estimate, and the internal `_timescaledb_*` and `timescaledb_information` schemas, with one table per chunk, are hidden from the sidebar (the SQL editor can still query them). A hypertable with compression enabled cannot be read with `ctid` or `xmin`, so without a primary key it is read-only. Give it one that includes the time column, such as `PRIMARY KEY (id, time)`, to edit it.

Values travel as PostgreSQL's own text format in both directions, the same text `psql` shows. Each edit is one `UPDATE … WHERE <key> RETURNING …`. A paste, clear or undo sends all its changes in one transaction. It is rejected if the row no longer matches exactly one record.

A cell cut in the grid comes with the md5 of its full value. Before such a cell is overwritten by a paste, a clear or an undo, the browser loads the full value and checks the hash, so the conflict check below and undo always work on the whole value.

Edits never silently overwrite someone else's work. Each edit also sends the values you saw in the cells you changed. If someone else has changed one of those cells since, the edit is refused and you are asked to refresh. Undo works the same way: it reverts a cell only if the cell still holds what your change wrote.

In the default order (by primary key, or by `ctid`), a page next to one already loaded is fetched from that page's first or last key instead of by OFFSET. Scrolling on from any point in a table of millions of rows therefore costs the same as at the top. A jump far down with the scrollbar, or a sort of your own, still uses OFFSET. The page is cut out first, and cut values and hashes are computed only for the rows sent.

If the database drops a connection (restart, failover, a killed backend), the request answers 503 so the browser can try again. Grid reads retry once on a fresh connection on their own.

Writes are addressed by record, not by position on screen. An edit still lands on its record if a refresh or a new sort moved that record, and edits to one record are saved in the order you made them. A paste or clear first loads every row it covers, so rows that were not on screen are not skipped. Copying and pasting within airadminer keeps NULL apart from empty text. JSON is re-indented for editing without touching numbers, strings or key order. The cell then shows the value the database returned, so triggers and defaults are reflected right away.
