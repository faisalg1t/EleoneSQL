package sqlparser

import "testing"

func TestParseCreateTable(t *testing.T) {
	stmt, err := Parse(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT UNIQUE)`)
	if err != nil {
		t.Fatal(err)
	}
	ct, ok := stmt.(*CreateTableStmt)
	if !ok {
		t.Fatalf("wrong type %T", stmt)
	}
	if ct.Table != "users" || len(ct.Columns) != 3 {
		t.Fatalf("unexpected: %+v", ct)
	}
	if !ct.Columns[0].PrimaryKey || !ct.Columns[1].NotNull || !ct.Columns[2].Unique {
		t.Fatalf("flags not parsed: %+v", ct.Columns)
	}
}

func TestParseInsertMultiRow(t *testing.T) {
	stmt, err := Parse(`INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')`)
	if err != nil {
		t.Fatal(err)
	}
	ins := stmt.(*InsertStmt)
	if len(ins.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(ins.Rows))
	}
}

func TestParseSelectWhereOrderLimit(t *testing.T) {
	stmt, err := Parse(`SELECT a, b FROM t WHERE a > 5 AND b = 'x' ORDER BY a DESC LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.Items) != 2 || sel.Where == nil || len(sel.OrderBy) != 1 || !sel.HasLim || sel.Limit != 10 {
		t.Fatalf("unexpected: %+v", sel)
	}
}

func TestParseJoin(t *testing.T) {
	stmt, err := Parse(`SELECT * FROM a JOIN b ON a.id = b.a_id WHERE a.x = 1`)
	if err != nil {
		t.Fatal(err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.Joins) != 1 || sel.Joins[0].Table != "b" {
		t.Fatalf("unexpected: %+v", sel)
	}
}

func TestParseUpdateDelete(t *testing.T) {
	if _, err := Parse(`UPDATE t SET a = 1, b = 'x' WHERE id = 5`); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(`DELETE FROM t WHERE id = 5`); err != nil {
		t.Fatal(err)
	}
}

func TestParseTxnControl(t *testing.T) {
	for _, sql := range []string{"BEGIN", "BEGIN TRANSACTION", "COMMIT", "ROLLBACK"} {
		if _, err := Parse(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func TestParseExpressionPrecedence(t *testing.T) {
	stmt, err := Parse(`SELECT * FROM t WHERE a + 1 = 2 * 3`)
	if err != nil {
		t.Fatal(err)
	}
	sel := stmt.(*SelectStmt)
	be, ok := sel.Where.(*BinaryExpr)
	if !ok || be.Op != "=" {
		t.Fatalf("unexpected where: %+v", sel.Where)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []string{
		"SELECT FROM",
		"CREATE TABLE",
		"INSERT INTO",
		"SELECT * FROM t WHERE",
	}
	for _, c := range cases {
		if _, err := Parse(c); err == nil {
			t.Fatalf("expected error for %q", c)
		}
	}
}
