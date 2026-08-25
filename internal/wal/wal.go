// Package wal implements EleoneSQL's write-ahead log.
//
// Every row/index mutation is written to the log before EleoneSQL applies
// it to the corresponding b-tree (the b-tree write itself is not
// individually fsynced; only the log is, and only at commit). Each log
// record carries both the before-image and the after-image of the key it
// touches, which lets recovery run in two passes:
//
//  1. Redo every operation belonging to a transaction that reached a
//     Commit record, applying its after-images. This repairs the case
//     where the b-tree write hadn't reached disk yet when the crash hit.
//  2. Undo every operation belonging to a transaction that reached
//     neither a Commit nor a Rollback record — i.e. the process crashed
//     mid-transaction — applying its before-images in reverse order.
//     This repairs the opposite case: EleoneSQL applies writes to the
//     b-tree live as a transaction runs (for read-your-own-writes), so an
//     abandoned transaction's writes may already be sitting on disk with
//     no in-memory undo log left to reverse them.
//
// Because both passes only ever replay idempotent Put/Delete operations,
// running redo and undo in sequence converges on the correct state
// regardless of exactly how far each individual page write had gotten
// before the crash.
package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

type RecordType byte

const (
	RecBegin RecordType = iota + 1
	RecPut
	RecDelete
	RecCommit
	RecRollback
)

// Op is one buffered mutation, used both when writing and when replaying.
type Op struct {
	Type    RecordType // RecPut or RecDelete
	Table   string
	Key     []byte
	Val     []byte // after-image; unused for RecDelete
	HadPrev bool
	PrevVal []byte // before-image, valid when HadPrev
}

// WAL is an append-only log file plus the machinery to replay and
// checkpoint it.
type WAL struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
}

// Open opens (creating if necessary) the WAL file at path for appending.
func Open(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("wal: open %s: %w", path, err)
	}
	return &WAL{path: path, f: f, w: bufio.NewWriter(f)}, nil
}

func (w *WAL) writeRecord(typ RecordType, txnID uint64, payload []byte) error {
	rec := make([]byte, 0, 4+1+8+len(payload)+4)
	body := make([]byte, 1+8+len(payload))
	body[0] = byte(typ)
	binary.BigEndian.PutUint64(body[1:], txnID)
	copy(body[9:], payload)

	crc := crc32.ChecksumIEEE(body)
	rec = append(rec, make([]byte, 4)...)
	binary.BigEndian.PutUint32(rec[0:4], uint32(len(body)))
	rec = append(rec, body...)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc)
	rec = append(rec, crcBuf[:]...)

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(rec); err != nil {
		return err
	}
	// Flush to the OS on every record, not just at commit: a record must
	// be written-ahead of the data-page write it describes so that even a
	// bare process kill (as opposed to an OS/power-loss crash, which needs
	// the fsync in Commit below) can't lose it while the corresponding
	// b-tree mutation survives. This is what makes the log a true
	// *write-ahead* log rather than just an at-commit audit trail.
	return w.w.Flush()
}

func (w *WAL) Begin(txnID uint64) error { return w.writeRecord(RecBegin, txnID, nil) }

// Put logs a write of key -> newVal. hadPrev/prevVal is the before-image
// (what the key held immediately before this write, if anything), used to
// undo the write during recovery if the transaction is later found to have
// been abandoned.
func (w *WAL) Put(txnID uint64, table string, key, newVal []byte, hadPrev bool, prevVal []byte) error {
	payload := encodeOp(table, key, newVal, hadPrev, prevVal)
	return w.writeRecord(RecPut, txnID, payload)
}

// Delete logs a removal of key, whose value immediately beforehand was
// prevVal (always present, since you can only delete a key that exists).
func (w *WAL) Delete(txnID uint64, table string, key, prevVal []byte) error {
	payload := encodeOp(table, key, nil, true, prevVal)
	return w.writeRecord(RecDelete, txnID, payload)
}

func (w *WAL) Rollback(txnID uint64) error { return w.writeRecord(RecRollback, txnID, nil) }

// Commit writes the commit record and fsyncs the log, the durability
// linchpin: once this returns nil, the transaction survives a crash.
func (w *WAL) Commit(txnID uint64) error {
	if err := w.writeRecord(RecCommit, txnID, nil); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.f.Sync()
}

func encodeOp(table string, key, newVal []byte, hadPrev bool, prevVal []byte) []byte {
	buf := make([]byte, 0, 2+len(table)+4+len(key)+4+len(newVal)+1+4+len(prevVal))
	writeChunk16 := func(b []byte) {
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(b)))
		buf = append(buf, l[:]...)
		buf = append(buf, b...)
	}
	writeChunk32 := func(b []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(b)))
		buf = append(buf, l[:]...)
		buf = append(buf, b...)
	}
	writeChunk16([]byte(table))
	writeChunk32(key)
	writeChunk32(newVal)
	if hadPrev {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	writeChunk32(prevVal)
	return buf
}

func decodeOp(b []byte) (table string, key, newVal []byte, hadPrev bool, prevVal []byte, err error) {
	readChunk16 := func() ([]byte, error) {
		if len(b) < 2 {
			return nil, io.ErrUnexpectedEOF
		}
		l := int(binary.BigEndian.Uint16(b))
		b = b[2:]
		if len(b) < l {
			return nil, io.ErrUnexpectedEOF
		}
		v := b[:l]
		b = b[l:]
		return v, nil
	}
	readChunk32 := func() ([]byte, error) {
		if len(b) < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		l := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		if len(b) < l {
			return nil, io.ErrUnexpectedEOF
		}
		v := b[:l]
		b = b[l:]
		return v, nil
	}
	tb, err := readChunk16()
	if err != nil {
		return
	}
	table = string(tb)
	if key, err = readChunk32(); err != nil {
		return
	}
	key = append([]byte(nil), key...)
	if newVal, err = readChunk32(); err != nil {
		return
	}
	newVal = append([]byte(nil), newVal...)
	if len(b) < 1 {
		err = io.ErrUnexpectedEOF
		return
	}
	hadPrev = b[0] == 1
	b = b[1:]
	if prevVal, err = readChunk32(); err != nil {
		return
	}
	prevVal = append([]byte(nil), prevVal...)
	return
}

// Replay scans the WAL from the start and reconstructs the correct
// post-crash state via apply: transactions that reached a Commit record
// are redone (after-images, in commit order); transactions that reached
// neither Commit nor Rollback are undone (before-images, reverse order,
// after every commit has been redone). Rolled-back transactions are
// ignored either way. A truncated/corrupt trailing record (torn write
// from a crash) stops the scan without error, since everything before it
// is still valid.
func Replay(path string, apply func(op Op) error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	pending := map[uint64][]Op{}

	for {
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			break // EOF or torn write: stop, nothing further is trustworthy
		}
		bodyLen := binary.BigEndian.Uint32(lenBuf)
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(r, body); err != nil {
			break
		}
		crcBuf := make([]byte, 4)
		if _, err := io.ReadFull(r, crcBuf); err != nil {
			break
		}
		wantCRC := binary.BigEndian.Uint32(crcBuf)
		if crc32.ChecksumIEEE(body) != wantCRC {
			break // corrupt tail record
		}
		if len(body) < 9 {
			break
		}
		typ := RecordType(body[0])
		txnID := binary.BigEndian.Uint64(body[1:9])
		payload := body[9:]

		switch typ {
		case RecBegin:
			// no-op marker

		case RecPut, RecDelete:
			table, key, newVal, hadPrev, prevVal, derr := decodeOp(payload)
			if derr != nil {
				break
			}
			pending[txnID] = append(pending[txnID], Op{
				Type: typ, Table: table, Key: key, Val: newVal,
				HadPrev: hadPrev, PrevVal: prevVal,
			})

		case RecRollback:
			delete(pending, txnID)

		case RecCommit:
			for _, op := range pending[txnID] {
				redo := op
				if op.Type == RecDelete {
					redo.Type = RecDelete
				}
				if err := applyRedo(apply, redo); err != nil {
					return err
				}
			}
			delete(pending, txnID)
		}
	}

	// Anything still pending reached neither Commit nor Rollback: the
	// process crashed mid-transaction. Undo its writes, most recent first.
	for _, ops := range pending {
		for i := len(ops) - 1; i >= 0; i-- {
			if err := applyUndo(apply, ops[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyRedo(apply func(Op) error, op Op) error {
	switch op.Type {
	case RecPut:
		return apply(Op{Type: RecPut, Table: op.Table, Key: op.Key, Val: op.Val})
	case RecDelete:
		return apply(Op{Type: RecDelete, Table: op.Table, Key: op.Key})
	}
	return nil
}

// applyUndo reverses one abandoned operation: a Put (or Delete) that had a
// before-image is reverted by restoring it; a Put that had no before-image
// (the key was newly created by this transaction) is reverted by deleting
// the key.
func applyUndo(apply func(Op) error, op Op) error {
	if op.HadPrev {
		return apply(Op{Type: RecPut, Table: op.Table, Key: op.Key, Val: op.PrevVal})
	}
	return apply(Op{Type: RecDelete, Table: op.Table, Key: op.Key})
}

// Checkpoint truncates the WAL to empty. Callers must ensure the underlying
// data file has been fsynced first, so every committed operation the WAL
// described is now durably reflected in the table pages.
func (w *WAL) Checkpoint() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return w.f.Sync()
}

// Close flushes and closes the WAL file.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.f.Close()
}
