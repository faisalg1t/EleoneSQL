// Package storage implements EleoneSQL's on-disk page manager and B+Tree.
//
// File layout:
//
//	Page 0 is the file header page. All other pages are either B+Tree
//	pages (leaf or internal) or free-list pages. Every page is exactly
//	PageSize bytes, which keeps offset arithmetic simple and lets us use
//	os.File.ReadAt/WriteAt for lock-free positional I/O.
package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
)

const (
	// PageSize is the fixed size, in bytes, of every page in an EleoneSQL
	// data file. 4096 matches the common OS/filesystem block size, which
	// keeps single-page writes atomic on most platforms.
	PageSize = 4096

	headerMagic       = uint32(0x456C6553) // "EleS"
	headerFormatMajor = uint16(1)

	// Header page (page 0) layout offsets.
	hdrMagicOff     = 0
	hdrVersionOff   = 4
	hdrPageCountOff = 6
	hdrFreeListOff  = 10
	hdrCatalogOff   = 14
	hdrReservedOff  = 18
)

var ErrClosed = errors.New("storage: pager is closed")

// PageID identifies a page within a file. 0 is reserved for the header page.
type PageID uint32

// Pager owns the underlying file and hands out fixed-size pages. It is safe
// for concurrent use; callers are still responsible for higher-level
// concurrency control (see internal/txn), the Pager only guarantees that
// individual page reads/writes are not corrupted by concurrent access.
type Pager struct {
	mu        sync.Mutex
	file      *os.File
	pageCount uint32
	freeList  PageID // head of the on-disk free list (0 = empty)
	catalog   PageID // root page of the catalog b-tree, 0 until created
	closed    bool
}

// Open opens (creating if necessary) a database file at path.
func Open(path string) (*Pager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	p := &Pager{file: f}
	if fi.Size() == 0 {
		// Brand new file: write header page.
		p.pageCount = 1
		if err := p.writeHeader(); err != nil {
			f.Close()
			return nil, err
		}
	} else {
		if err := p.readHeader(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return p, nil
}

func (p *Pager) readHeader() error {
	buf := make([]byte, PageSize)
	if _, err := p.file.ReadAt(buf, 0); err != nil {
		return fmt.Errorf("storage: read header: %w", err)
	}
	magic := binary.BigEndian.Uint32(buf[hdrMagicOff:])
	if magic != headerMagic {
		return errors.New("storage: not an EleoneSQL data file (bad magic)")
	}
	p.pageCount = binary.BigEndian.Uint32(buf[hdrPageCountOff:])
	p.freeList = PageID(binary.BigEndian.Uint32(buf[hdrFreeListOff:]))
	p.catalog = PageID(binary.BigEndian.Uint32(buf[hdrCatalogOff:]))
	return nil
}

func (p *Pager) writeHeader() error {
	buf := make([]byte, PageSize)
	binary.BigEndian.PutUint32(buf[hdrMagicOff:], headerMagic)
	binary.BigEndian.PutUint16(buf[hdrVersionOff:], headerFormatMajor)
	binary.BigEndian.PutUint32(buf[hdrPageCountOff:], p.pageCount)
	binary.BigEndian.PutUint32(buf[hdrFreeListOff:], uint32(p.freeList))
	binary.BigEndian.PutUint32(buf[hdrCatalogOff:], uint32(p.catalog))
	_, err := p.file.WriteAt(buf, 0)
	return err
}

// CatalogRoot returns the root page of the catalog b-tree (0 if none yet).
func (p *Pager) CatalogRoot() PageID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.catalog
}

// SetCatalogRoot persists the catalog b-tree root page id.
func (p *Pager) SetCatalogRoot(id PageID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.catalog = id
	return p.writeHeader()
}

// ReadPage reads page id into a freshly allocated buffer.
func (p *Pager) ReadPage(id PageID) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	buf := make([]byte, PageSize)
	off := int64(id) * PageSize
	if _, err := p.file.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("storage: read page %d: %w", id, err)
	}
	return buf, nil
}

// WritePage writes buf (must be exactly PageSize bytes) to page id.
func (p *Pager) WritePage(id PageID, buf []byte) error {
	if len(buf) != PageSize {
		return fmt.Errorf("storage: page buffer must be %d bytes, got %d", PageSize, len(buf))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	off := int64(id) * PageSize
	_, err := p.file.WriteAt(buf, off)
	return err
}

// AllocatePage returns a fresh, zeroed page, reusing a freed page if the
// free list is non-empty, otherwise extending the file.
func (p *Pager) AllocatePage() (PageID, error) {
	p.mu.Lock()
	if p.freeList != 0 {
		id := p.freeList
		p.mu.Unlock()
		buf, err := p.ReadPage(id)
		if err != nil {
			return 0, err
		}
		next := PageID(binary.BigEndian.Uint32(buf[0:4]))
		p.mu.Lock()
		p.freeList = next
		if err := p.writeHeader(); err != nil {
			p.mu.Unlock()
			return 0, err
		}
		p.mu.Unlock()
		zero := make([]byte, PageSize)
		if err := p.WritePage(id, zero); err != nil {
			return 0, err
		}
		return id, nil
	}
	id := PageID(p.pageCount)
	p.pageCount++
	if err := p.writeHeader(); err != nil {
		p.mu.Unlock()
		return 0, err
	}
	p.mu.Unlock()
	zero := make([]byte, PageSize)
	if err := p.WritePage(id, zero); err != nil {
		return 0, err
	}
	return id, nil
}

// FreePage returns a page to the free list for reuse.
func (p *Pager) FreePage(id PageID) error {
	p.mu.Lock()
	head := p.freeList
	p.mu.Unlock()

	buf := make([]byte, PageSize)
	binary.BigEndian.PutUint32(buf[0:4], uint32(head))
	if err := p.WritePage(id, buf); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.freeList = id
	return p.writeHeader()
}

// Sync flushes all writes to stable storage.
func (p *Pager) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	return p.file.Sync()
}

// Close syncs and closes the underlying file.
func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if err := p.file.Sync(); err != nil {
		p.file.Close()
		return err
	}
	return p.file.Close()
}

// PageCount returns the total number of pages currently in the file
// (including free ones), for diagnostics.
func (p *Pager) PageCount() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pageCount
}
