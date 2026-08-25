package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/faisalg1t/EleoneSQL/internal/txn"
)

func newTestSession(t *testing.T) (*Session, string, string) {
	t.Helper()
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "test.edb")
	walPath := filepath.Join(dir, "test.wal")
	store, err := txn.Open(dataPath, walPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return NewSession(store), dataPath, walPath
}

func mustExec(t *testing.T, s *Session, sql string) *Result {
	t.Helper()
	res, err := s.Execute(sql)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return res
}

func TestCreateInsertSelect(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INTEGER)`)
	mustExec(t, s, `INSERT INTO users (id, name, age) VALUES (1, 'Alice', 30), (2, 'Bob', 25)`)

	res := mustExec(t, s, `SELECT * FROM users WHERE age > 26`)
	if len(res.Rows) != 1 || res.Rows[0][1].S != "Alice" {
		t.Fatalf("unexpected result: %+v", res.Rows)
	}

	res = mustExec(t, s, `SELECT name FROM users ORDER BY age ASC`)
	if len(res.Rows) != 2 || res.Rows[0][0].S != "Bob" || res.Rows[1][0].S != "Alice" {
		t.Fatalf("unexpected order: %+v", res.Rows)
	}
}

func TestPrimaryKeyUniqueness(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (1, 'a')`)
	if _, err := s.Execute(`INSERT INTO t (id, v) VALUES (1, 'b')`); err == nil {
		t.Fatal("expected duplicate primary key to fail")
	}
}

func TestUpdateAndDelete(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	for i := 1; i <= 5; i++ {
		mustExec(t, s, fmt.Sprintf(`INSERT INTO t (id, v) VALUES (%d, 'v%d')`, i, i))
	}
	res := mustExec(t, s, `UPDATE t SET v = 'updated' WHERE id >= 3`)
	if res.RowsAffected != 3 {
		t.Fatalf("expected 3 rows affected, got %d", res.RowsAffected)
	}
	res = mustExec(t, s, `SELECT v FROM t WHERE id = 4`)
	if res.Rows[0][0].S != "updated" {
		t.Fatalf("update didn't apply: %+v", res.Rows)
	}
	res = mustExec(t, s, `DELETE FROM t WHERE id < 3`)
	if res.RowsAffected != 2 {
		t.Fatalf("expected 2 rows deleted, got %d", res.RowsAffected)
	}
	res = mustExec(t, s, `SELECT COUNT(*) FROM t`)
	if res.Rows[0][0].I != 3 {
		t.Fatalf("expected 3 remaining rows, got %v", res.Rows[0][0])
	}
}

func TestExplicitTransactionRollback(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (1, 'a')`)

	mustExec(t, s, `BEGIN`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (2, 'b')`)
	mustExec(t, s, `UPDATE t SET v = 'changed' WHERE id = 1`)
	mustExec(t, s, `ROLLBACK`)

	res := mustExec(t, s, `SELECT COUNT(*) FROM t`)
	if res.Rows[0][0].I != 1 {
		t.Fatalf("rollback of insert failed, count=%v", res.Rows[0][0])
	}
	res = mustExec(t, s, `SELECT v FROM t WHERE id = 1`)
	if res.Rows[0][0].S != "a" {
		t.Fatalf("rollback of update failed: %+v", res.Rows)
	}
}

func TestExplicitTransactionCommit(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExec(t, s, `BEGIN`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (1, 'a')`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (2, 'b')`)
	mustExec(t, s, `COMMIT`)

	res := mustExec(t, s, `SELECT COUNT(*) FROM t`)
	if res.Rows[0][0].I != 2 {
		t.Fatalf("commit failed, count=%v", res.Rows[0][0])
	}
}

func TestJoin(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT)`)
	mustExec(t, s, `CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT, author_id INTEGER)`)
	mustExec(t, s, `INSERT INTO authors (id, name) VALUES (1, 'Orwell'), (2, 'Tolkien')`)
	mustExec(t, s, `INSERT INTO books (id, title, author_id) VALUES (1, '1984', 1), (2, 'Animal Farm', 1), (3, 'The Hobbit', 2)`)

	res := mustExec(t, s, `SELECT b.title, a.name FROM books b JOIN authors a ON b.author_id = a.id WHERE a.name = 'Orwell' ORDER BY b.title ASC`)
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(res.Rows), res.Rows)
	}
	if res.Rows[0][0].S != "1984" || res.Rows[1][0].S != "Animal Farm" {
		t.Fatalf("unexpected join result: %+v", res.Rows)
	}
}

func TestCrashRecoveryReplaysCommittedTxn(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "test.edb")
	walPath := filepath.Join(dir, "test.wal")

	store, err := txn.Open(dataPath, walPath)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(store)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (1, 'a')`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (2, 'b')`)
	// Simulate a crash: close the pager's file handle directly without a
	// clean WAL checkpoint, so the WAL still holds the committed inserts.
	store.Pager.Close()
	store.WAL.Close()

	// Reopen: recovery should replay the WAL and reconstruct the rows.
	store2, err := txn.Open(dataPath, walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	s2 := NewSession(store2)
	res := mustExec(t, s2, `SELECT COUNT(*) FROM t`)
	if res.Rows[0][0].I != 2 {
		t.Fatalf("expected 2 rows after recovery, got %v", res.Rows[0][0])
	}
	res = mustExec(t, s2, `SELECT v FROM t WHERE id = 2`)
	if len(res.Rows) != 1 || res.Rows[0][0].S != "b" {
		t.Fatalf("recovered row wrong: %+v", res.Rows)
	}
}

func TestCrashRecoverySkipsUncommittedTxn(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "test.edb")
	walPath := filepath.Join(dir, "test.wal")

	store, err := txn.Open(dataPath, walPath)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(store)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (1, 'a')`)

	// Start (but do not commit) a transaction, then simulate a crash.
	mustExec(t, s, `BEGIN`)
	mustExec(t, s, `INSERT INTO t (id, v) VALUES (2, 'b')`)
	store.Pager.Close()
	store.WAL.Close()

	store2, err := txn.Open(dataPath, walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	s2 := NewSession(store2)
	res := mustExec(t, s2, `SELECT COUNT(*) FROM t`)
	if res.Rows[0][0].I != 1 {
		t.Fatalf("expected uncommitted insert to be discarded, count=%v", res.Rows[0][0])
	}
}

func TestNotNullConstraint(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL)`)
	if _, err := s.Execute(`INSERT INTO t (id) VALUES (1)`); err == nil {
		t.Fatal("expected NOT NULL violation to fail")
	}
}

func TestDropTable(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	mustExec(t, s, `DROP TABLE t`)
	if _, err := s.Execute(`SELECT * FROM t`); err == nil {
		t.Fatal("expected select on dropped table to fail")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "test.edb")
	walPath := filepath.Join(dir, "test.wal")

	store, err := txn.Open(dataPath, walPath)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(store)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	for i := 0; i < 200; i++ {
		mustExec(t, s, fmt.Sprintf(`INSERT INTO t (id, v) VALUES (%d, 'row%d')`, i, i))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := txn.Open(dataPath, walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	s2 := NewSession(store2)
	res := mustExec(t, s2, `SELECT COUNT(*) FROM t`)
	if res.Rows[0][0].I != 200 {
		t.Fatalf("expected 200 rows, got %v", res.Rows[0][0])
	}
}

func TestCreateIndexAndUniqueConstraint(t *testing.T) {
	s, _, _ := newTestSession(t)
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, email TEXT)`)
	mustExec(t, s, `INSERT INTO t (id, email) VALUES (1, 'a@x.com')`)
	mustExec(t, s, `CREATE UNIQUE INDEX idx_email ON t(email)`)
	if _, err := s.Execute(`INSERT INTO t (id, email) VALUES (2, 'a@x.com')`); err == nil {
		t.Fatal("expected unique index violation to fail")
	}
	mustExec(t, s, `INSERT INTO t (id, email) VALUES (3, 'b@x.com')`)
	res := mustExec(t, s, `SELECT id FROM t WHERE email = 'b@x.com'`)
	if len(res.Rows) != 1 || res.Rows[0][0].I != 3 {
		t.Fatalf("index lookup failed: %+v", res.Rows)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}
