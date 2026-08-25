# EleoneSQL

EleoneSQL is a single-node relational database engine written in Go from
scratch: on-disk B+Tree storage, write-ahead logging with crash recovery,
a SQL parser/executor, transactions, secondary indexes, and a
client/server protocol with a CLI — no external dependencies, no C code,
no third-party SQL/storage libraries.

**What this is not:** a drop-in PostgreSQL replacement. Postgres represents
several decades and person-centuries of engineering — a cost-based query
optimizer, MVCC with true concurrent readers/writers, streaming
replication, extensive SQL surface area (window functions, CTEs, stored
procedures, full-text search...), and a large hardening/fuzzing corpus.
This project implements the *core mechanics* of a real relational database
— storage, durability, transactions, query execution — correctly and
tested, and is upfront in the [Roadmap](#roadmap) below about everything
it deliberately leaves out to stay in scope.

Every claim below is backed by tests in this repo (`go test ./...`) and by
manual end-to-end verification against the running server, including
literally `kill -9`-ing the server mid-transaction and confirming recovery
does the right thing (see [Verifying it yourself](#verifying-it-yourself)).

## Features

- **Storage engine**: a real on-disk B+Tree (`internal/storage`) with
  4KB slotted pages, node splitting, free-page reuse, and ordered range
  cursors — not a wrapper around an in-memory map.
- **Durability**: write-ahead logging (`internal/wal`) with before/after
  images per operation. On restart, committed transactions are redone and
  transactions that were mid-flight when the process died are undone —
  verified against real `SIGKILL`s, not just simulated ones.
- **Transactions**: `BEGIN` / `COMMIT` / `ROLLBACK`, serializable via a
  global write lock, with full rollback (including of secondary index
  changes) via an in-memory undo log during normal operation and via the
  WAL's before-images if the process dies first.
- **SQL surface**: `CREATE/DROP TABLE`, `CREATE [UNIQUE] INDEX`,
  `INSERT` (multi-row), `SELECT` (`WHERE`, `ORDER BY`, `LIMIT`,
  `JOIN ... ON`, `COUNT(*)`), `UPDATE`, `DELETE`, `SHOW TABLES`. Typed
  columns (`INTEGER`, `FLOAT`, `TEXT`, `BOOLEAN`), `PRIMARY KEY`,
  `UNIQUE`, `NOT NULL`.
- **Indexes**: secondary B+Tree indexes, used automatically for
  equality lookups (`WHERE col = ...`) instead of a full table scan.
- **Client/server**: a TCP server (`eleonesqld`) and an interactive CLI
  (`eleonesql`) speaking a small line-based protocol
  (`internal/server/wire`) — plus an `-embed` mode for the CLI to talk to
  a data file directly, in-process, without a server.

## Quick start

```sh
go build -o eleonesqld ./cmd/eleonesqld
go build -o eleonesql  ./cmd/eleonesql

./eleonesqld -data mydb.edb -wal mydb.wal -addr :5432 &

./eleonesql -addr localhost:5432
eleonesql> CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INTEGER);
eleonesql> INSERT INTO users (id, name, age) VALUES (1, 'Alice', 30), (2, 'Bob', 25);
eleonesql> SELECT * FROM users WHERE age > 26 ORDER BY age DESC;
eleonesql> exit;
```

Or run a single statement non-interactively:

```sh
./eleonesql -addr localhost:5432 -c "SELECT COUNT(*) FROM users"
```

Or skip the server entirely and open a file in-process:

```sh
./eleonesql -embed mydb.edb -c "SELECT * FROM users"
```

## Architecture

```
cmd/eleonesqld        server binary
cmd/eleonesql          CLI client (networked or -embed)
internal/storage       pager (fixed 4KB pages) + B+Tree
internal/wal            write-ahead log (redo + undo recovery)
internal/catalog       schema storage, typed values, row encoding
internal/sqlparser      hand-written lexer + recursive-descent parser
internal/txn            transaction manager tying pager+catalog+WAL together
internal/engine         statement executor (DDL/DML/SELECT)
internal/server         TCP server
internal/server/wire    client/server line protocol
```

Data flow for a write: `engine` parses SQL into an AST via `sqlparser`,
resolves table/column metadata via `catalog`, and issues `Put`/`Delete`
calls to a `txn.Txn`, which writes a before/after-image record to the
`wal.WAL` (flushed to the OS immediately, fsynced at commit), applies the
change to the relevant `storage.BTree` (row heap and/or secondary
indexes), and keeps an in-memory undo entry in case of `ROLLBACK`.

### Why a global write lock instead of MVCC

Real concurrent transactions need multi-version concurrency control:
readers see a consistent snapshot without blocking writers, writers don't
block each other unless they touch the same rows, and you need a garbage
collector for old row versions. That's a substantial chunk of engineering
on its own. EleoneSQL instead serializes all writes (and, for simplicity,
all statements) behind one lock per `Store`. You get correct, easy-to-
reason-about ACID semantics; you don't get write concurrency. This is the
single biggest architectural simplification in this codebase, and the
first thing a "make this actually production-grade" effort should
replace.

### Why DDL isn't WAL-logged

`CREATE`/`DROP TABLE` and `CREATE INDEX` mutate the catalog directly and
then synchronously `fsync` the data file, rather than going through the
WAL like row-level DML does. This keeps the WAL's target-resolution logic
(row heap vs. named secondary index) from also having to model
in-flight schema changes, at the cost of DDL being a bit slower (a
blocking fsync per statement) and not participating in a surrounding
`BEGIN`/`COMMIT` block (DDL always takes effect immediately, like in
MySQL's autocommit behavior for schema changes, rather than being
transactional like in PostgreSQL).

### Known limitations (by design, for this version)

- **No query optimizer.** Beyond the single-column equality-index fast
  path, queries are full table scans; `JOIN` is always nested-loop.
- **No streaming execution.** `SELECT`/`UPDATE`/`DELETE` materialize the
  full row set they're operating over in memory before filtering.
- **Delete doesn't rebalance the B+Tree.** Space from deleted rows isn't
  reclaimed (no page merging on underflow). A future `VACUUM` could
  compact this; for now it's a documented space-amplification tradeoff
  chosen for a much smaller, easier-to-verify implementation.
- **Single-column `PRIMARY KEY` only**, no composite keys.
- **No `ALTER TABLE`**, no foreign keys, no `GROUP BY` beyond bare
  `COUNT(*)`, no subqueries, no views, no user-defined types/functions.
- **The wire protocol is EleoneSQL-specific**, not PostgreSQL's; you
  can't point `psql` at it.

## Testing

```sh
go test ./...          # unit + integration tests
go vet ./...
```

Notable tests:
- `internal/storage`: B+Tree correctness under sequential/random inserts
  spanning many page splits, cursor ordering, deletes, and pager
  persistence across a real close/reopen.
- `internal/engine`: end-to-end SQL (joins, transactions, constraints)
  and — the two that matter most for a database — WAL recovery tests that
  simulate a crash mid-transaction and verify the abandoned write is
  undone, and a crash after commit-but-before-checkpoint and verify the
  committed write is redone.

## Verifying it yourself

Beyond the automated tests, this behavior has been manually verified
against the actual running server binary, including a real `kill -9`:

1. Start `eleonesqld`, open a raw socket, send `BEGIN` then an `INSERT`,
   leave the transaction open (no `COMMIT`).
2. `kill -9` the server process.
3. Restart it and query the table: the uncommitted row is **not** there —
   recovery undid it using the WAL's before-images, even though the row
   had already been physically written to the data file's B+Tree pages
   before the kill.
4. Repeat, but send `COMMIT` before the kill: the row **is** there after
   restart, recovered via WAL redo even though the process died before
   the next checkpoint.

This is the same class of test real database test suites run (`jepsen`-
style crash testing, at a much smaller scale here) and is what caught a
real bug during development: WAL records were being buffered in
user-space (Go's `bufio.Writer`) and only flushed to the OS at commit
time, so a process kill between a row write and its transaction's commit
lost the log record needed to undo it, while the row write itself (a real
`write(2)` syscall against the data file) survived. The fix — flush every
WAL record to the OS immediately, and reserve `fsync` for commit — is
what makes the log actually *write-ahead*.

## Roadmap

Ordered roughly by what would matter most for real production use:

1. **MVCC** — replace the global write lock with proper multi-version
   concurrency control for concurrent readers/writers.
2. **Query planner** — cost-based join ordering, use of indexes for
   range predicates and `ORDER BY`, not just point equality.
3. **Streaming execution** — cursor-based pipelines instead of
   materializing full row sets.
4. **B+Tree page merging on delete/underflow**, plus a `VACUUM` command.
5. **Replication** — streaming WAL shipping to standbys.
6. **Richer SQL** — composite keys, foreign keys, `GROUP BY`/`HAVING`
   with real aggregates, subqueries, `ALTER TABLE`.
7. **PostgreSQL wire protocol compatibility**, so existing drivers/tools
   work unmodified.
8. **Auth, TLS, and connection pooling** for the server.

## License

MIT — see [LICENSE](LICENSE).
