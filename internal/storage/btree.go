package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Page layout (both node types share the same header):
//
//	offset 0   : pageType byte (leafPage | internalPage)
//	offset 1-2 : numCells uint16
//	offset 3-6 : sibling pointer uint32
//	                 leaf:     next-leaf PageID (0 = none), for range scans
//	                 internal: rightmost child PageID
//	offset 7-8 : cellContentStart uint16, cell data grows downward from PageSize
//	offset 9.. : numCells * 2-byte cell offsets (kept sorted by key), then
//	             free space, then cell bodies packed from the end of the page.
//
// Leaf cell body:     [keyLen u16][valLen u16][key][value]
// Internal cell body: [keyLen u16][childPageID u32][key]
//   an internal cell's child holds all keys strictly less than the cell's key.

const (
	leafPage     = byte(1)
	internalPage = byte(2)

	hdrPageType    = 0
	hdrNumCells    = 1
	hdrSibling     = 3
	hdrContentTop  = 7
	cellPtrArrayAt = 9
)

var (
	ErrKeyNotFound = errors.New("storage: key not found")
	ErrKeyTooLarge = errors.New("storage: key/value too large for a page")
)

// BTree is an on-disk B+Tree mapping arbitrary byte-string keys (compared
// lexicographically) to arbitrary byte-string values.
type BTree struct {
	pager *Pager
	root  PageID
}

// OpenBTree opens an existing tree rooted at root, or creates a new empty
// tree (allocating a root leaf page) if root == 0.
func OpenBTree(pager *Pager, root PageID) (*BTree, error) {
	t := &BTree{pager: pager, root: root}
	if root == 0 {
		id, err := pager.AllocatePage()
		if err != nil {
			return nil, err
		}
		buf := newNodeBuf(leafPage)
		if err := pager.WritePage(id, buf); err != nil {
			return nil, err
		}
		t.root = id
	}
	return t, nil
}

// Root returns the tree's current root page id, e.g. for persisting into a
// catalog entry.
func (t *BTree) Root() PageID { return t.root }

func newNodeBuf(pageType byte) []byte {
	buf := make([]byte, PageSize)
	buf[hdrPageType] = pageType
	binary.BigEndian.PutUint16(buf[hdrNumCells:], 0)
	binary.BigEndian.PutUint32(buf[hdrSibling:], 0)
	binary.BigEndian.PutUint16(buf[hdrContentTop:], PageSize)
	return buf
}

type node struct {
	id  PageID
	buf []byte
}

func (n *node) pageType() byte    { return n.buf[hdrPageType] }
func (n *node) isLeaf() bool      { return n.buf[hdrPageType] == leafPage }
func (n *node) numCells() int     { return int(binary.BigEndian.Uint16(n.buf[hdrNumCells:])) }
func (n *node) setNumCells(v int) { binary.BigEndian.PutUint16(n.buf[hdrNumCells:], uint16(v)) }
func (n *node) sibling() PageID   { return PageID(binary.BigEndian.Uint32(n.buf[hdrSibling:])) }
func (n *node) setSibling(id PageID) {
	binary.BigEndian.PutUint32(n.buf[hdrSibling:], uint32(id))
}
func (n *node) contentTop() int { return int(binary.BigEndian.Uint16(n.buf[hdrContentTop:])) }
func (n *node) setContentTop(v int) {
	binary.BigEndian.PutUint16(n.buf[hdrContentTop:], uint16(v))
}

func (n *node) cellOffset(i int) int {
	return int(binary.BigEndian.Uint16(n.buf[cellPtrArrayAt+2*i:]))
}
func (n *node) setCellOffset(i, off int) {
	binary.BigEndian.PutUint16(n.buf[cellPtrArrayAt+2*i:], uint16(off))
}

// freeSpace returns the number of unused bytes available in the page.
func (n *node) freeSpace() int {
	ptrArrayEnd := cellPtrArrayAt + 2*n.numCells()
	return n.contentTop() - ptrArrayEnd
}

// leafKV returns the key/value stored in cell i of a leaf node.
func (n *node) leafKV(i int) (key, val []byte) {
	off := n.cellOffset(i)
	klen := int(binary.BigEndian.Uint16(n.buf[off:]))
	vlen := int(binary.BigEndian.Uint16(n.buf[off+2:]))
	key = n.buf[off+4 : off+4+klen]
	val = n.buf[off+4+klen : off+4+klen+vlen]
	return
}

// internalKC returns the key and left-child of cell i of an internal node.
func (n *node) internalKC(i int) (key []byte, child PageID) {
	off := n.cellOffset(i)
	klen := int(binary.BigEndian.Uint16(n.buf[off:]))
	child = PageID(binary.BigEndian.Uint32(n.buf[off+2:]))
	key = n.buf[off+6 : off+6+klen]
	return
}

// findIndex returns the index of the first cell whose key is >= key
// (i.e. the standard lower_bound), and whether an exact match was found.
func (n *node) findIndex(key []byte) (idx int, exact bool) {
	lo, hi := 0, n.numCells()
	for lo < hi {
		mid := (lo + hi) / 2
		var k []byte
		if n.isLeaf() {
			k, _ = n.leafKV(mid)
		} else {
			k, _ = n.internalKC(mid)
		}
		c := bytes.Compare(k, key)
		if c == 0 {
			return mid, true
		} else if c < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, false
}

// insertCellAt inserts a raw cell body at logical index idx, shifting later
// pointers. Returns false if there isn't room.
func (n *node) insertCellAt(idx int, body []byte) bool {
	need := len(body) + 2
	if n.freeSpace() < need {
		return false
	}
	top := n.contentTop() - len(body)
	copy(n.buf[top:], body)
	n.setContentTop(top)

	nc := n.numCells()
	// Shift pointer array right to make room at idx.
	for i := nc; i > idx; i-- {
		n.setCellOffset(i, n.cellOffset(i-1))
	}
	n.setCellOffset(idx, top)
	n.setNumCells(nc + 1)
	return true
}

func (n *node) removeCellAt(idx int) {
	nc := n.numCells()
	for i := idx; i < nc-1; i++ {
		n.setCellOffset(i, n.cellOffset(i+1))
	}
	n.setNumCells(nc - 1)
	// Note: this does not reclaim the now-unreferenced cell body bytes.
	// Pages are compacted lazily on split; a bounded amount of internal
	// fragmentation is an accepted tradeoff for implementation simplicity.
}

func leafCellBody(key, val []byte) []byte {
	body := make([]byte, 4+len(key)+len(val))
	binary.BigEndian.PutUint16(body[0:], uint16(len(key)))
	binary.BigEndian.PutUint16(body[2:], uint16(len(val)))
	copy(body[4:], key)
	copy(body[4+len(key):], val)
	return body
}

func internalCellBody(key []byte, child PageID) []byte {
	body := make([]byte, 6+len(key))
	binary.BigEndian.PutUint16(body[0:], uint16(len(key)))
	binary.BigEndian.PutUint32(body[2:], uint32(child))
	copy(body[6:], key)
	return body
}

func (t *BTree) readNode(id PageID) (*node, error) {
	buf, err := t.pager.ReadPage(id)
	if err != nil {
		return nil, err
	}
	return &node{id: id, buf: buf}, nil
}

func (t *BTree) writeNode(n *node) error {
	return t.pager.WritePage(n.id, n.buf)
}

const maxKeyValue = PageSize/4 - 16 // conservative cap so 2 cells always fit a fresh page

// Get looks up key and returns its value.
func (t *BTree) Get(key []byte) ([]byte, error) {
	n, err := t.readNode(t.root)
	if err != nil {
		return nil, err
	}
	for !n.isLeaf() {
		idx, exact := n.findIndex(key)
		var child PageID
		if exact {
			// keys in internal cells act as lower_bound boundaries; an
			// exact match belongs to the right side (>=), so descend past it.
			idx++
		}
		if idx >= n.numCells() {
			child = n.sibling()
		} else {
			_, child = n.internalKC(idx)
		}
		n, err = t.readNode(child)
		if err != nil {
			return nil, err
		}
	}
	idx, exact := n.findIndex(key)
	if !exact {
		return nil, ErrKeyNotFound
	}
	_, v := n.leafKV(idx)
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

// Has reports whether key exists.
func (t *BTree) Has(key []byte) (bool, error) {
	_, err := t.Get(key)
	if errors.Is(err, ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// splitResult describes a node split that must be linked into the parent.
type splitResult struct {
	promoteKey []byte
	newRight   PageID
}

// Put inserts or updates key -> value.
func (t *BTree) Put(key, val []byte) error {
	if len(key) > maxKeyValue || len(val) > maxKeyValue {
		return ErrKeyTooLarge
	}
	split, err := t.putRec(t.root, key, val)
	if err != nil {
		return err
	}
	if split != nil {
		// Root split: create a new root.
		newRootID, err := t.pager.AllocatePage()
		if err != nil {
			return err
		}
		root := &node{id: newRootID, buf: newNodeBuf(internalPage)}
		body := internalCellBody(split.promoteKey, t.root)
		root.insertCellAt(0, body)
		root.setSibling(split.newRight)
		if err := t.writeNode(root); err != nil {
			return err
		}
		t.root = newRootID
	}
	return nil
}

func (t *BTree) putRec(id PageID, key, val []byte) (*splitResult, error) {
	n, err := t.readNode(id)
	if err != nil {
		return nil, err
	}

	if n.isLeaf() {
		idx, exact := n.findIndex(key)
		if exact {
			n.removeCellAt(idx)
		}
		body := leafCellBody(key, val)
		if n.insertCellAt(idx, body) {
			return nil, t.writeNode(n)
		}
		return t.splitLeafAndInsert(n, idx, body)
	}

	idx, exact := n.findIndex(key)
	if exact {
		idx++
	}
	var child PageID
	if idx >= n.numCells() {
		child = n.sibling()
	} else {
		_, child = n.internalKC(idx)
	}
	split, err := t.putRec(child, key, val)
	if err != nil || split == nil {
		return nil, err
	}
	body := internalCellBody(split.promoteKey, child)
	if idx >= n.numCells() {
		// child was the rightmost pointer; new right becomes rightmost.
		if n.insertCellAt(idx, body) {
			n.setSibling(split.newRight)
			return nil, t.writeNode(n)
		}
		return t.splitInternalAndInsert(n, idx, body, split.newRight, true)
	}
	if n.insertCellAt(idx, body) {
		// fix the following cell's child pointer to point at newRight
		return nil, t.rewriteChildAt(n, idx+1, split.newRight)
	}
	return t.splitInternalAndInsert(n, idx, body, split.newRight, false)
}

// rewriteChildAt overwrites the child pointer of an existing internal cell
// in place (used after inserting a new sibling cell to its left).
func (t *BTree) rewriteChildAt(n *node, idx int, child PageID) error {
	off := n.cellOffset(idx)
	binary.BigEndian.PutUint32(n.buf[off+2:], uint32(child))
	return t.writeNode(n)
}

func (t *BTree) splitLeafAndInsert(n *node, insertIdx int, insertBody []byte) (*splitResult, error) {
	// Gather all existing cells + the new one in order, then rebuild two pages.
	cells := make([][]byte, 0, n.numCells()+1)
	for i := 0; i < n.numCells(); i++ {
		k, v := n.leafKV(i)
		if i == insertIdx {
			cells = append(cells, insertBody)
		}
		kc := append([]byte(nil), k...)
		vc := append([]byte(nil), v...)
		cells = append(cells, leafCellBody(kc, vc))
	}
	if insertIdx >= n.numCells() {
		cells = append(cells, insertBody)
	}

	mid := len(cells) / 2
	rightID, err := t.pager.AllocatePage()
	if err != nil {
		return nil, err
	}
	left := &node{id: n.id, buf: newNodeBuf(leafPage)}
	right := &node{id: rightID, buf: newNodeBuf(leafPage)}
	for i, c := range cells {
		if i < mid {
			left.insertCellAt(left.numCells(), c)
		} else {
			right.insertCellAt(right.numCells(), c)
		}
	}
	right.setSibling(n.sibling())
	left.setSibling(rightID)

	if err := t.writeNode(left); err != nil {
		return nil, err
	}
	if err := t.writeNode(right); err != nil {
		return nil, err
	}
	promoteKey, _ := right.leafKV(0)
	return &splitResult{promoteKey: append([]byte(nil), promoteKey...), newRight: rightID}, nil
}

func (t *BTree) splitInternalAndInsert(n *node, insertIdx int, insertBody []byte, newRightChild PageID, insertedWasRightmost bool) (*splitResult, error) {
	type kc struct {
		key   []byte
		child PageID
	}
	cells := make([]kc, 0, n.numCells()+1)
	for i := 0; i < n.numCells(); i++ {
		k, c := n.internalKC(i)
		if i == insertIdx {
			ik, ic := internalCellKeyChild(insertBody)
			cells = append(cells, kc{ik, ic})
		}
		cells = append(cells, kc{append([]byte(nil), k...), c})
	}
	if insertIdx >= n.numCells() {
		ik, ic := internalCellKeyChild(insertBody)
		cells = append(cells, kc{ik, ic})
	}
	oldRightmost := n.sibling()

	// If the split arose from a non-rightmost child, the cell immediately
	// after the inserted one must have its child pointer updated to
	// newRightChild (it used to point at the pre-split child).
	if !insertedWasRightmost {
		if insertIdx+1 < len(cells) {
			cells[insertIdx+1].child = newRightChild
		} else {
			oldRightmost = newRightChild
		}
	}

	mid := len(cells) / 2
	promote := cells[mid]

	rightID, err := t.pager.AllocatePage()
	if err != nil {
		return nil, err
	}
	left := &node{id: n.id, buf: newNodeBuf(internalPage)}
	right := &node{id: rightID, buf: newNodeBuf(internalPage)}
	for i := 0; i < mid; i++ {
		left.insertCellAt(left.numCells(), internalCellBody(cells[i].key, cells[i].child))
	}
	for i := mid + 1; i < len(cells); i++ {
		right.insertCellAt(right.numCells(), internalCellBody(cells[i].key, cells[i].child))
	}
	right.setSibling(oldRightmost)
	if insertedWasRightmost {
		left.setSibling(promote.child)
	} else {
		left.setSibling(promote.child)
	}

	if err := t.writeNode(left); err != nil {
		return nil, err
	}
	if err := t.writeNode(right); err != nil {
		return nil, err
	}
	return &splitResult{promoteKey: promote.key, newRight: rightID}, nil
}

func internalCellKeyChild(body []byte) ([]byte, PageID) {
	klen := int(binary.BigEndian.Uint16(body[0:]))
	child := PageID(binary.BigEndian.Uint32(body[2:]))
	key := append([]byte(nil), body[6:6+klen]...)
	return key, child
}

// Delete removes key from the tree. EleoneSQL uses a simplified delete that
// does not rebalance/merge underfull pages; this trades a small amount of
// long-run space amplification under heavy delete workloads for a much
// smaller, easier-to-verify implementation. Space can be reclaimed with a
// future VACUUM (tracked in the roadmap).
func (t *BTree) Delete(key []byte) error {
	n, err := t.readNode(t.root)
	if err != nil {
		return err
	}
	path := []PageID{}
	for !n.isLeaf() {
		path = append(path, n.id)
		idx, exact := n.findIndex(key)
		if exact {
			idx++
		}
		var child PageID
		if idx >= n.numCells() {
			child = n.sibling()
		} else {
			_, child = n.internalKC(idx)
		}
		n, err = t.readNode(child)
		if err != nil {
			return err
		}
	}
	idx, exact := n.findIndex(key)
	if !exact {
		return ErrKeyNotFound
	}
	n.removeCellAt(idx)
	return t.writeNode(n)
}

// Cursor iterates leaf entries in ascending key order.
type Cursor struct {
	t       *BTree
	n       *node
	i       int
	started bool
}

// NewCursor returns a cursor positioned before the first entry with key >=
// startKey (nil means "from the beginning").
func (t *BTree) NewCursor(startKey []byte) (*Cursor, error) {
	n, err := t.readNode(t.root)
	if err != nil {
		return nil, err
	}
	for !n.isLeaf() {
		var child PageID
		if startKey == nil {
			_, child = n.internalKC(0)
			if n.numCells() == 0 {
				child = n.sibling()
			}
		} else {
			idx, exact := n.findIndex(startKey)
			if exact {
				idx++
			}
			if idx >= n.numCells() {
				child = n.sibling()
			} else {
				_, child = n.internalKC(idx)
			}
		}
		n, err = t.readNode(child)
		if err != nil {
			return nil, err
		}
	}
	i := 0
	if startKey != nil {
		i, _ = n.findIndex(startKey)
	}
	return &Cursor{t: t, n: n, i: i}, nil
}

// Next advances the cursor and returns the next key/value pair, or
// (nil, nil, false, nil) when iteration is exhausted.
func (c *Cursor) Next() (key, val []byte, ok bool, err error) {
	for {
		if c.i < c.n.numCells() {
			k, v := c.n.leafKV(c.i)
			c.i++
			return append([]byte(nil), k...), append([]byte(nil), v...), true, nil
		}
		next := c.n.sibling()
		if next == 0 {
			return nil, nil, false, nil
		}
		c.n, err = c.t.readNode(next)
		if err != nil {
			return nil, nil, false, err
		}
		c.i = 0
	}
}

// DebugString dumps a human-readable tree summary (used in tests/tools).
func (t *BTree) DebugString() (string, error) {
	return t.debugNode(t.root, 0)
}

func (t *BTree) debugNode(id PageID, depth int) (string, error) {
	n, err := t.readNode(id)
	if err != nil {
		return "", err
	}
	pad := ""
	for i := 0; i < depth; i++ {
		pad += "  "
	}
	s := ""
	if n.isLeaf() {
		s += fmt.Sprintf("%sleaf#%d (%d cells)\n", pad, id, n.numCells())
	} else {
		s += fmt.Sprintf("%sinternal#%d (%d cells)\n", pad, id, n.numCells())
		for i := 0; i < n.numCells(); i++ {
			_, child := n.internalKC(i)
			sub, err := t.debugNode(child, depth+1)
			if err != nil {
				return "", err
			}
			s += sub
		}
		sub, err := t.debugNode(n.sibling(), depth+1)
		if err != nil {
			return "", err
		}
		s += sub
	}
	return s, nil
}
