package diff

import "testing"

func TestDirectoryIsNotEmptyFileOrTombstone(t *testing.T) {
	directory := &Manifest{ID: "directory", Directory: true}
	file := &Manifest{ID: "file"}
	deleted := &Manifest{ID: "deleted", Tombstone: true}
	if got := Plan(nil, directory, file); got.Action != Conflict {
		t.Fatal(got)
	}
	if got := Plan(directory, directory, deleted); got.Action != Pull {
		t.Fatal(got)
	}
	if got := Plan(directory, file, deleted); got.Action != Conflict {
		t.Fatal(got)
	}
	if got := Plan(nil, nil, directory); got.Action != Pull {
		t.Fatal(got)
	}
	if got := Plan(nil, directory, &Manifest{ID: "another directory", Directory: true}); got.Action != Noop {
		t.Fatal(got)
	}
	if blocks := MissingBlocks(directory, nil); len(blocks) != 0 {
		t.Fatal(blocks)
	}
	for _, bad := range []*Manifest{{Directory: true, Tombstone: true}, {Directory: true, Size: 1}, {Directory: true, Blocks: []Block{{Size: 1}}}} {
		if err := bad.Validate(1); err == nil {
			t.Fatal("invalid directory accepted", bad)
		}
	}
}
