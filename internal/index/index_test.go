package index

import (
	"path/filepath"
	"testing"
)

func TestPersistenceAndGenerations(t *testing.T) {
	p := filepath.Join(t.TempDir(), "index.db")
	d, err := Open(p, "/root")
	if err != nil {
		t.Fatal(err)
	}
	r := Record{Path: "a", Fingerprint: Fingerprint{Size: 1}, Scan: 1}
	if err = d.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	r.Scan = 2
	if err = d.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	got, ok, err := d.Get("a")
	if err != nil || !ok || got.Generation != 1 || !got.Dirty {
		t.Fatalf("%+v %v", got, err)
	}
	if err = d.Put([]Record{r}, true); err != nil {
		t.Fatal(err)
	}
	id, _ := d.NextScan()
	d.Close()
	d, err = Open(p, "/root")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	next, _ := d.NextScan()
	if next <= id {
		t.Fatal("scan sequence reset")
	}
	got, _, _ = d.Get("a")
	if got.Generation != 2 {
		t.Fatal(got)
	}
}
func TestPrefixPagination(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "i.db"), "/root")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, p := range []string{"a", "a-other", "a/one", "a/two", "abc", "b"} {
		if err = d.Put([]Record{{Path: p}}, false); err != nil {
			t.Fatal(err)
		}
	}
	page, err := d.PagePrefix("a/", "", 1)
	if err != nil || len(page) != 1 || page[0].Path != "a/one" {
		t.Fatalf("%v %v", page, err)
	}
	page, err = d.PagePrefix("a/", page[0].Path, 10)
	if err != nil || len(page) != 1 || page[0].Path != "a/two" {
		t.Fatalf("%v %v", page, err)
	}
}
