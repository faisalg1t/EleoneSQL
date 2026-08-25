package storage

import (
	"fmt"
	"math/rand"
	"os"
	"testing"
)

func tempPager(t *testing.T) *Pager {
	t.Helper()
	f, err := os.CreateTemp("", "eleonesql-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	path := f.Name()
	t.Cleanup(func() { os.Remove(path) })
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestBTreeBasicPutGet(t *testing.T) {
	p := tempPager(t)
	bt, err := OpenBTree(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := bt.Put([]byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := bt.Put([]byte("k2"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	v, err := bt.Get([]byte("k1"))
	if err != nil || string(v) != "v1" {
		t.Fatalf("got %q, %v", v, err)
	}
	// update
	if err := bt.Put([]byte("k1"), []byte("v1-updated")); err != nil {
		t.Fatal(err)
	}
	v, err = bt.Get([]byte("k1"))
	if err != nil || string(v) != "v1-updated" {
		t.Fatalf("got %q, %v", v, err)
	}
	if _, err := bt.Get([]byte("missing")); err != ErrKeyNotFound {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

func TestBTreeManyInsertsAndSplits(t *testing.T) {
	p := tempPager(t)
	bt, err := OpenBTree(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 5000
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key-%06d", i))
		val := []byte(fmt.Sprintf("value-%d-payload", i))
		if err := bt.Put(key, val); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key-%06d", i))
		want := fmt.Sprintf("value-%d-payload", i)
		got, err := bt.Get(key)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("get %d: want %q got %q", i, want, got)
		}
	}
}

func TestBTreeRandomOrderInsert(t *testing.T) {
	p := tempPager(t)
	bt, err := OpenBTree(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 3000
	perm := rand.New(rand.NewSource(42)).Perm(n)
	for _, i := range perm {
		key := []byte(fmt.Sprintf("k%06d", i))
		if err := bt.Put(key, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("k%06d", i))
		got, err := bt.Get(key)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		want := fmt.Sprintf("v%d", i)
		if string(got) != want {
			t.Fatalf("get %d: want %q got %q", i, want, got)
		}
	}
}

func TestBTreeCursorOrdering(t *testing.T) {
	p := tempPager(t)
	bt, err := OpenBTree(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 2000
	perm := rand.New(rand.NewSource(7)).Perm(n)
	for _, i := range perm {
		key := []byte(fmt.Sprintf("k%06d", i))
		if err := bt.Put(key, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := bt.NewCursor(nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	prev := ""
	for {
		k, v, ok, err := cur.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if string(k) <= prev && count > 0 {
			t.Fatalf("cursor not in ascending order at %d: prev=%q cur=%q", count, prev, k)
		}
		prev = string(k)
		_ = v
		count++
	}
	if count != n {
		t.Fatalf("expected %d entries, got %d", n, count)
	}
}

func TestBTreeDelete(t *testing.T) {
	p := tempPager(t)
	bt, err := OpenBTree(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		key := []byte(fmt.Sprintf("k%06d", i))
		if err := bt.Put(key, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 500; i += 2 {
		key := []byte(fmt.Sprintf("k%06d", i))
		if err := bt.Delete(key); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	for i := 0; i < 500; i++ {
		key := []byte(fmt.Sprintf("k%06d", i))
		v, err := bt.Get(key)
		if i%2 == 0 {
			if err != ErrKeyNotFound {
				t.Fatalf("expected deleted key %d gone, got %v %v", i, v, err)
			}
		} else {
			if err != nil {
				t.Fatalf("expected key %d present: %v", i, err)
			}
		}
	}
}

func TestPagerPersistence(t *testing.T) {
	f, err := os.CreateTemp("", "eleonesql-persist-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := OpenBTree(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		key := []byte(fmt.Sprintf("k%06d", i))
		if err := bt.Put(key, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	root := bt.Root()
	if err := p.SetCatalogRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	bt2, err := OpenBTree(p2, p2.CatalogRoot())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		key := []byte(fmt.Sprintf("k%06d", i))
		want := fmt.Sprintf("v%d", i)
		got, err := bt2.Get(key)
		if err != nil {
			t.Fatalf("get %d after reopen: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("get %d after reopen: want %q got %q", i, want, got)
		}
	}
}
