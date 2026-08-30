// Package engine turns parsed SQL statements into effects against a
// txn.Store: schema changes, row mutations, and query results.
package engine

import (
	"fmt"

	"github.com/faisaljs/EleoneSQL/internal/catalog"
	"github.com/faisaljs/EleoneSQL/internal/sqlparser"
	"github.com/faisaljs/EleoneSQL/internal/txn"
)

// Result is the outcome of executing one statement.
type Result struct {
	Columns      []string
	Rows         [][]catalog.Value
	RowsAffected int
	Message      string
}

// Session represents one client's connection: it may have an open explicit
// transaction (BEGIN ... COMMIT/ROLLBACK) spanning several statements.
type Session struct {
	Store *txn.Store
	txn   *txn.Txn
}

func NewSession(store *txn.Store) *Session { return &Session{Store: store} }

// InTxn reports whether an explicit transaction is currently open.
func (s *Session) InTxn() bool { return s.txn != nil }

// Close rolls back any open transaction (e.g. on client disconnect).
func (s *Session) Close() {
	if s.txn != nil {
		s.txn.Rollback()
		s.txn = nil
	}
}

// Execute parses and runs a single SQL statement.
func (s *Session) Execute(sql string) (*Result, error) {
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		return nil, err
	}
	return s.executeStmt(stmt)
}

func (s *Session) executeStmt(stmt sqlparser.Statement) (*Result, error) {
	switch st := stmt.(type) {
	case *sqlparser.BeginStmt:
		if s.txn != nil {
			return nil, fmt.Errorf("engine: a transaction is already open")
		}
		s.txn = s.Store.Begin()
		return &Result{Message: "BEGIN"}, nil

	case *sqlparser.CommitStmt:
		if s.txn == nil {
			return nil, fmt.Errorf("engine: no transaction is open")
		}
		err := s.txn.Commit()
		s.txn = nil
		if err != nil {
			return nil, err
		}
		if err := s.Store.MaybeCheckpoint(2000); err != nil {
			return nil, err
		}
		return &Result{Message: "COMMIT"}, nil

	case *sqlparser.RollbackStmt:
		if s.txn == nil {
			return nil, fmt.Errorf("engine: no transaction is open")
		}
		err := s.txn.Rollback()
		s.txn = nil
		if err != nil {
			return nil, err
		}
		return &Result{Message: "ROLLBACK"}, nil

	case *sqlparser.CreateTableStmt:
		return s.execCreateTable(st)
	case *sqlparser.DropTableStmt:
		return s.execDropTable(st)
	case *sqlparser.CreateIndexStmt:
		return s.execCreateIndex(st)
	case *sqlparser.ShowTablesStmt:
		return s.execShowTables()

	case *sqlparser.InsertStmt:
		return s.runInTxn(func(t *txn.Txn) (*Result, error) { return s.execInsert(t, st) })
	case *sqlparser.UpdateStmt:
		return s.runInTxn(func(t *txn.Txn) (*Result, error) { return s.execUpdate(t, st) })
	case *sqlparser.DeleteStmt:
		return s.runInTxn(func(t *txn.Txn) (*Result, error) { return s.execDelete(t, st) })
	case *sqlparser.SelectStmt:
		return s.runInTxn(func(t *txn.Txn) (*Result, error) { return s.execSelect(t, st) })

	default:
		return nil, fmt.Errorf("engine: unsupported statement type %T", stmt)
	}
}

// runInTxn executes fn against the session's explicit transaction if one is
// open, otherwise wraps it in a one-statement implicit transaction that
// auto-commits on success and rolls back on error.
func (s *Session) runInTxn(fn func(t *txn.Txn) (*Result, error)) (*Result, error) {
	if s.txn != nil {
		return fn(s.txn)
	}
	t := s.Store.Begin()
	res, err := fn(t)
	if err != nil {
		t.Rollback()
		return nil, err
	}
	if err := t.Commit(); err != nil {
		return nil, err
	}
	if err := s.Store.MaybeCheckpoint(2000); err != nil {
		return nil, err
	}
	return res, nil
}
