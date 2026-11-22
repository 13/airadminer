# Features

[← README](../README.md)

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
- Filters are built per type (is, contains, <, ≥, is empty, is checked …) and can include raw SQL conditions. A text field's value box suggests the values in that column, most frequent first: a table of up to 200,000 rows is queried exactly, and a bigger one uses the planner's statistics (`pg_stats`, merged across partitions or hypertable chunks), so it never scans the whole table. Sorting can use several fields, and there is a search box that matches any field.
- Hidden fields, column widths, sorts, filters, row height and fit to screen are remembered for each table in your browser.
- The address bar carries the search, filters, sorts and hidden fields, so you can bookmark a view or send the link to someone. A link opens exactly that view; a link without one opens the view saved in that browser. Column widths, row height and fit to screen stay per browser. A raw SQL filter in a link runs only after the person opening it agrees, because it runs with their privileges. With `-no-sql` it is dropped.

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
