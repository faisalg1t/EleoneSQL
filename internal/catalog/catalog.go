package catalog

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/faisaljs/EleoneSQL/internal/storage"
)

// ColumnDef describes one column of a table.
type ColumnDef struct {
	Name       string
	Type       Type
	PrimaryKey bool
	Unique     bool
	NotNull    bool
}

// IndexDef describes a secondary index on a single column.
type IndexDef struct {
	Name   string
	Column string
	Unique bool
	Root   storage.PageID
}

// TableDef is a table's full schema plus the root page of its row heap
// (a b-tree keyed by row id) and any secondary indexes.
type TableDef struct {
	Name    string
	Columns []ColumnDef
	Root    storage.PageID
	NextRow uint64
	Indexes []IndexDef
}

func (t *TableDef) ColumnIndex(name string) int {
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func (t *TableDef) ColumnTypes() []Type {
	types := make([]Type, len(t.Columns))
	for i, c := range t.Columns {
		types[i] = c.Type
	}
	return types
}

// PrimaryKeyColumn returns the index of the declared primary key column, or
// -1 if the table has none.
func (t *TableDef) PrimaryKeyColumn() int {
	for i, c := range t.Columns {
		if c.PrimaryKey {
			return i
		}
	}
	return -1
}

// Catalog stores and retrieves table schemas. It is backed by a dedicated
// b-tree (the "system catalog") whose root page id lives in the file
// header, so schema changes are just ordinary b-tree mutations and get the
// same crash-recovery guarantees as table data.
type Catalog struct {
	mu     sync.RWMutex
	pager  *storage.Pager
	sysBT  *storage.BTree
	tables map[string]*TableDef
}

// Open loads (or initializes) the catalog for the given pager.
func Open(pager *storage.Pager) (*Catalog, error) {
	bt, err := storage.OpenBTree(pager, pager.CatalogRoot())
	if err != nil {
		return nil, err
	}
	if pager.CatalogRoot() == 0 {
		if err := pager.SetCatalogRoot(bt.Root()); err != nil {
			return nil, err
		}
	}
	c := &Catalog{pager: pager, sysBT: bt, tables: map[string]*TableDef{}}
	if err := c.loadAll(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Catalog) loadAll() error {
	cur, err := c.sysBT.NewCursor(nil)
	if err != nil {
		return err
	}
	for {
		_, v, ok, err := cur.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		td, err := decodeTableDef(v)
		if err != nil {
			return err
		}
		c.tables[td.Name] = td
	}
	return nil
}

// Table returns the schema for name, or (nil, false) if it doesn't exist.
func (c *Catalog) Table(name string) (*TableDef, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	td, ok := c.tables[name]
	return td, ok
}

// TableNames returns all known table names.
func (c *Catalog) TableNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.tables))
	for n := range c.tables {
		out = append(out, n)
	}
	return out
}

// CreateTable registers a brand new table, allocating its row heap.
func (c *Catalog) CreateTable(name string, cols []ColumnDef) (*TableDef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[name]; exists {
		return nil, fmt.Errorf("catalog: table %q already exists", name)
	}
	bt, err := storage.OpenBTree(c.pager, 0)
	if err != nil {
		return nil, err
	}
	td := &TableDef{Name: name, Columns: cols, Root: bt.Root(), NextRow: 1}
	if err := c.persist(td); err != nil {
		return nil, err
	}
	c.tables[name] = td
	return td, nil
}

// DropTable removes a table's schema entry. The underlying pages are not
// reclaimed (documented limitation; see VACUUM in the roadmap).
func (c *Catalog) DropTable(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[name]; !exists {
		return fmt.Errorf("catalog: table %q does not exist", name)
	}
	if err := c.sysBT.Delete([]byte(name)); err != nil {
		return err
	}
	delete(c.tables, name)
	return nil
}

// SaveTable persists an updated TableDef (e.g. after NextRow advances or an
// index is added).
func (c *Catalog) SaveTable(td *TableDef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persist(td)
}

func (c *Catalog) persist(td *TableDef) error {
	if err := c.sysBT.Put([]byte(td.Name), encodeTableDef(td)); err != nil {
		return err
	}
	// The system b-tree's own root can move when it splits; keep the file
	// header in sync so a reopen finds it.
	return c.pager.SetCatalogRoot(c.sysBT.Root())
}

// --- serialization ---------------------------------------------------

func encodeTableDef(td *TableDef) []byte {
	var buf bytes.Buffer
	writeString(&buf, td.Name)
	binary.Write(&buf, binary.BigEndian, uint32(td.Root))
	binary.Write(&buf, binary.BigEndian, td.NextRow)
	binary.Write(&buf, binary.BigEndian, uint16(len(td.Columns)))
	for _, col := range td.Columns {
		writeString(&buf, col.Name)
		buf.WriteByte(byte(col.Type))
		flags := byte(0)
		if col.PrimaryKey {
			flags |= 1
		}
		if col.Unique {
			flags |= 2
		}
		if col.NotNull {
			flags |= 4
		}
		buf.WriteByte(flags)
	}
	binary.Write(&buf, binary.BigEndian, uint16(len(td.Indexes)))
	for _, idx := range td.Indexes {
		writeString(&buf, idx.Name)
		writeString(&buf, idx.Column)
		flags := byte(0)
		if idx.Unique {
			flags |= 1
		}
		buf.WriteByte(flags)
		binary.Write(&buf, binary.BigEndian, uint32(idx.Root))
	}
	return buf.Bytes()
}

func decodeTableDef(data []byte) (*TableDef, error) {
	r := bytes.NewReader(data)
	name, err := readString(r)
	if err != nil {
		return nil, err
	}
	var root uint32
	if err := binary.Read(r, binary.BigEndian, &root); err != nil {
		return nil, err
	}
	var nextRow uint64
	if err := binary.Read(r, binary.BigEndian, &nextRow); err != nil {
		return nil, err
	}
	var ncols uint16
	if err := binary.Read(r, binary.BigEndian, &ncols); err != nil {
		return nil, err
	}
	cols := make([]ColumnDef, ncols)
	for i := range cols {
		n, err := readString(r)
		if err != nil {
			return nil, err
		}
		tb, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		flags, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		cols[i] = ColumnDef{
			Name:       n,
			Type:       Type(tb),
			PrimaryKey: flags&1 != 0,
			Unique:     flags&2 != 0,
			NotNull:    flags&4 != 0,
		}
	}
	var nidx uint16
	if err := binary.Read(r, binary.BigEndian, &nidx); err != nil {
		return nil, err
	}
	idxs := make([]IndexDef, nidx)
	for i := range idxs {
		n, err := readString(r)
		if err != nil {
			return nil, err
		}
		col, err := readString(r)
		if err != nil {
			return nil, err
		}
		flags, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		var iroot uint32
		if err := binary.Read(r, binary.BigEndian, &iroot); err != nil {
			return nil, err
		}
		idxs[i] = IndexDef{Name: n, Column: col, Unique: flags&1 != 0, Root: storage.PageID(iroot)}
	}
	return &TableDef{
		Name:    name,
		Columns: cols,
		Root:    storage.PageID(root),
		NextRow: nextRow,
		Indexes: idxs,
	}, nil
}

func writeString(buf *bytes.Buffer, s string) {
	binary.Write(buf, binary.BigEndian, uint16(len(s)))
	buf.WriteString(s)
}

func readString(r *bytes.Reader) (string, error) {
	var l uint16
	if err := binary.Read(r, binary.BigEndian, &l); err != nil {
		return "", err
	}
	b := make([]byte, l)
	if _, err := r.Read(b); err != nil {
		return "", err
	}
	return string(b), nil
}
