package index

import "testing"

func TestNativeCompletionPreservesLaterSameMetadataEvent(t *testing.T) {
	d := openSyncTest(t)
	r := Record{Path: "file", Fingerprint: Fingerprint{Size: 20}}
	if err := d.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	old, _, err := d.Get("file")
	if err != nil {
		t.Fatal(err)
	}
	// Explicit events invalidate even an unchanged cached fingerprint.
	if err := d.Put([]Record{r}, true); err != nil {
		t.Fatal(err)
	}
	if cleared, err := d.ClearObserved(old); err != nil || cleared {
		t.Fatal("newer event was cleared", cleared, err)
	}
	page, err := d.DirtyPage("", 1)
	if err != nil || len(page) != 1 {
		t.Fatal(page, err)
	}
	if cleared, err := d.ClearObserved(page[0]); err != nil || !cleared {
		t.Fatal(cleared, err)
	}
	page, err = d.DirtyPage("", 1)
	if err != nil || len(page) != 0 {
		t.Fatal(page, err)
	}
	if base, err := d.Base("file"); err != nil || base != nil {
		t.Fatal("native capture fabricated a replica base", base, err)
	}
}
