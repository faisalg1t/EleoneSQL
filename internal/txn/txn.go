// Package txn ties the pager, catalog and WAL together behind a simple
// transaction API. EleoneSQL uses a single global write lock (held for the
// duration of a transaction) rather than MVCC: this gives straightforward
// serializable isolation at the cost of write concurrency, a deliberate
// simplicity/robustness tradeoff for a first version — see the roadmap in
// the top-level README for what real concurrent transactions would need.
//
// Every mutation — whether to a table's row heap or to one of its
// secondary indexes — goes through Put/Delete below, which is keyed by a
// "target" string that both WAL replay and Rollback use to find the right
// b-tree again later: "T:<table>" for the row heap, "I:<table>:<index>"
// for a named secondary index. Because a single logical row write (INSERT
// with indexed columns) touches several b-trees, doing this generically
// lets one WAL transaction record cover row + index writes atomically.
package txn

import (
	"fmt"
	"strings"
	"sync"

	"github.com/faisalg1t/EleoneSQL/internal/catalog"
	"github.com/faisalg1t/EleoneSQL/internal/storage"
	"github.com/faisalg1t/EleoneSQL/internal/wal"
)

// Store bundles a database's on-disk state: page file, catalog, and WAL.
type Store struct {
	mu        sync.Mutex // global write lock: one active txn at a time
	Pager     *storage.Pager
	Catalog   *catalog.Catalog
	WAL       *wal.WAL
	nextTxnID uint64

	// writesSinceCheckpoint is used by the caller (server) to decide when
	// to checkpoint the WAL.
	writesSinceCheckpoint int
}

// TableTarget returns the WAL/undo target key for table's row heap.
func TableTarget(table string) string { return "T:" + table }

// IndexTarget returns the WAL/undo target key for a named secondary index.
func IndexTarget(table, index string) string { return "I:" + table + ":" + index }

// resolve looks up the b-tree root for a target key against the current
// catalog, along with a setter to persist a moved root.
func resolve(cat *catalog.Catalog, target string) (root storage.PageID, save func(storage.PageID) error, ok bool) {
	if strings.HasPrefix(target, "T:") {
		name := target[2:]
		td, exists := cat.Table(name)
		if !exists {
			return 0, nil, false
		}
		return td.Root, func(id storage.PageID) error {
			td.Root = id
			return cat.SaveTable(td)
		}, true
	}
	if strings.HasPrefix(target, "I:") {
		rest := target[2:]
		sep := strings.IndexByte(rest, ':')
		if sep < 0 {
			return 0, nil, false
		}
		tableName, idxName := rest[:sep], rest[sep+1:]
		td, exists := cat.Table(tableName)
		if !exists {
			return 0, nil, false
		}
		for i := range td.Indexes {
			if td.Indexes[i].Name == idxName {
				idx := i
				return td.Indexes[idx].Root, func(id storage.PageID) error {
					td.Indexes[idx].Root = id
					return cat.SaveTable(td)
				}, true
			}
		}
	}
	return 0, nil, false
}

// Open opens (or creates) a database at dataPath/walPath, replaying the WAL
// first to redo any committed-but-not-yet-checkpointed writes.
func Open(dataPath, walPath string) (*Store, error) {
	pager, err := storage.Open(dataPath)
	if err != nil {
		return nil, err
	}
	cat, err := catalog.Open(pager)
	if err != nil {
		return nil, err
	}
	// Recovery: redo every committed operation from the WAL. Put/Delete are
	// idempotent, so this is safe even if some of them already made it to
	// disk before the crash.
	if err := wal.Replay(walPath, func(op wal.Op) error {
		root, save, ok := resolve(cat, op.Table)
		if !ok {
			return nil // table/index was dropped after this op was logged; skip
		}
		bt, err := storage.OpenBTree(pager, root)
		if err != nil {
			return err
		}
		switch op.Type {
		case wal.RecPut:
			if err := bt.Put(op.Key, op.Val); err != nil {
				return err
			}
		case wal.RecDelete:
			if err := bt.Delete(op.Key); err != nil && err != storage.ErrKeyNotFound {
				return err
			}
		}
		if bt.Root() != root {
			return save(bt.Root())
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("txn: wal replay: %w", err)
	}
	if err := pager.Sync(); err != nil {
		return nil, err
	}

	w, err := wal.Open(walPath)
	if err != nil {
		return nil, err
	}
	// Replay is now durably applied to the data file; start the WAL fresh.
	if err := w.Checkpoint(); err != nil {
		return nil, err
	}

	return &Store{Pager: pager, Catalog: cat, WAL: w}, nil
}

func (s *Store) Close() error {
	if err := s.WAL.Close(); err != nil {
		return err
	}
	return s.Pager.Close()
}

// undoEntry records enough to reverse one mutation against one target tree.
type undoEntry struct {
	target  string
	key     []byte
	hadPrev bool
	prev    []byte
}

// Txn is an in-flight transaction. Not safe for concurrent use (a single
// connection drives at most one Txn at a time).
type Txn struct {
	store *Store
	id    uint64
	undo  []undoEntry
	done  bool
}

// Begin starts a new transaction, blocking until any other transaction
// finishes (single global writer).
func (s *Store) Begin() *Txn {
	s.mu.Lock()
	s.nextTxnID++
	id := s.nextTxnID
	s.WAL.Begin(id)
	return &Txn{store: s, id: id}
}

// Put writes key -> val into the b-tree identified by target (a table's
// row heap or one of its secondary indexes).
func (t *Txn) Put(target string, key, val []byte) error {
	root, save, ok := resolve(t.store.Catalog, target)
	if !ok {
		return fmt.Errorf("txn: unknown target %q", target)
	}
	bt, err := storage.OpenBTree(t.store.Pager, root)
	if err != nil {
		return err
	}
	prev, err := bt.Get(key)
	hadPrev := err == nil
	if err != nil && err != storage.ErrKeyNotFound {
		return err
	}
	if err := t.store.WAL.Put(t.id, target, key, val, hadPrev, prev); err != nil {
		return err
	}
	if err := bt.Put(key, val); err != nil {
		return err
	}
	if bt.Root() != root {
		if err := save(bt.Root()); err != nil {
			return err
		}
	}
	t.undo = append(t.undo, undoEntry{target: target, key: append([]byte(nil), key...), hadPrev: hadPrev, prev: prev})
	return nil
}

// Delete removes key from the b-tree identified by target.
func (t *Txn) Delete(target string, key []byte) error {
	root, save, ok := resolve(t.store.Catalog, target)
	if !ok {
		return fmt.Errorf("txn: unknown target %q", target)
	}
	bt, err := storage.OpenBTree(t.store.Pager, root)
	if err != nil {
		return err
	}
	prev, err := bt.Get(key)
	if err != nil {
		return err
	}
	if err := t.store.WAL.Delete(t.id, target, key, prev); err != nil {
		return err
	}
	if err := bt.Delete(key); err != nil {
		return err
	}
	if bt.Root() != root {
		if err := save(bt.Root()); err != nil {
			return err
		}
	}
	t.undo = append(t.undo, undoEntry{target: target, key: append([]byte(nil), key...), hadPrev: true, prev: prev})
	return nil
}

// Get reads key from the b-tree identified by target (read-your-own-writes
// within the txn).
func (t *Txn) Get(target string, key []byte) ([]byte, error) {
	root, _, ok := resolve(t.store.Catalog, target)
	if !ok {
		return nil, fmt.Errorf("txn: unknown target %q", target)
	}
	bt, err := storage.OpenBTree(t.store.Pager, root)
	if err != nil {
		return nil, err
	}
	return bt.Get(key)
}

// Cursor returns a b-tree cursor over the target starting at start (nil for
// the beginning).
func (t *Txn) Cursor(target string, start []byte) (*storage.Cursor, error) {
	root, _, ok := resolve(t.store.Catalog, target)
	if !ok {
		return nil, fmt.Errorf("txn: unknown target %q", target)
	}
	bt, err := storage.OpenBTree(t.store.Pager, root)
	if err != nil {
		return nil, err
	}
	return bt.NewCursor(start)
}

// Commit durably commits the transaction (fsyncs the WAL) and releases the
// global write lock.
func (t *Txn) Commit() error {
	if t.done {
		return nil
	}
	t.done = true
	defer t.store.mu.Unlock()
	if err := t.store.WAL.Commit(t.id); err != nil {
		return err
	}
	t.store.writesSinceCheckpoint += len(t.undo)
	return nil
}

// Rollback undoes every mutation made in this transaction (across every
// target it touched) and releases the global write lock.
func (t *Txn) Rollback() error {
	if t.done {
		return nil
	}
	t.done = true
	defer t.store.mu.Unlock()
	for i := len(t.undo) - 1; i >= 0; i-- {
		e := t.undo[i]
		root, save, ok := resolve(t.store.Catalog, e.target)
		if !ok {
			continue
		}
		bt, err := storage.OpenBTree(t.store.Pager, root)
		if err != nil {
			return err
		}
		if e.hadPrev {
			if err := bt.Put(e.key, e.prev); err != nil {
				return err
			}
		} else {
			if err := bt.Delete(e.key); err != nil && err != storage.ErrKeyNotFound {
				return err
			}
		}
		if bt.Root() != root {
			if err := save(bt.Root()); err != nil {
				return err
			}
		}
	}
	return t.store.WAL.Rollback(t.id)
}

// MaybeCheckpoint fsyncs the data file and truncates the WAL if enough
// writes have accumulated since the last checkpoint. Cheap to call after
// every commit; it's a no-op most of the time.
func (s *Store) MaybeCheckpoint(threshold int) error {
	if s.writesSinceCheckpoint < threshold {
		return nil
	}
	if err := s.Pager.Sync(); err != nil {
		return err
	}
	if err := s.WAL.Checkpoint(); err != nil {
		return err
	}
	s.writesSinceCheckpoint = 0
	return nil
}
