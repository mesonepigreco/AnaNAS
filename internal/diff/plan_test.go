package diff

import (
	"testing"

	"nas-sync/internal/hash"
)

func manifest(id string, content string) *Manifest {
	digest := hash.SumBytes([]byte(content))
	return &Manifest{ID: id, Size: int64(len(content)), Blocks: []Block{{Digest: digest, Size: int64(len(content))}}}
}

func TestPlanClassifiesBaseDivergence(t *testing.T) {
	base := manifest("base", "same")
	local := manifest("local", "local")
	remote := manifest("remote", "remote")
	cases := []struct {
		name   string
		base   *Manifest
		local  *Manifest
		remote *Manifest
		want   Action
	}{
		{"both unchanged", base, base, base, Noop},
		{"local only", base, local, base, Push},
		{"remote only", base, base, remote, Pull},
		{"both diverged", base, local, remote, Conflict},
		{"same content independent ids", base, manifest("local", "same"), manifest("remote", "same"), Noop},
		{"new local create", nil, local, nil, Push},
		{"new remote create", nil, nil, remote, Pull},
		{"independent creates conflict", nil, local, remote, Conflict},
		{"local delete", base, &Manifest{ID: "deleted-local", Tombstone: true}, base, Push},
		{"remote delete", base, base, &Manifest{ID: "deleted-remote", Tombstone: true}, Pull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Plan(tc.base, tc.local, tc.remote); got.Action != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestPlanNeverChoosesAConflictWinner(t *testing.T) {
	got := Plan(manifest("base", "base"), manifest("local", "left"), manifest("remote", "right"))
	if got.Action != Conflict || got.Reason == "" {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestMissingBlocksDeduplicatesAndPreservesOrder(t *testing.T) {
	a := hash.SumBytes([]byte("a"))
	b := hash.SumBytes([]byte("b"))
	local := &Manifest{Blocks: []Block{{Digest: a, Size: 1}, {Digest: b, Size: 1}, {Digest: a, Size: 1}}}
	got := MissingBlocks(local, map[hash.Digest]struct{}{b: {}})
	if len(got) != 1 || got[0].Digest != a {
		t.Fatalf("missing blocks: %+v", got)
	}
	if got := MissingBlocks(&Manifest{Tombstone: true}, nil); got != nil {
		t.Fatalf("tombstone needs no blocks: %+v", got)
	}
}

func TestManifestValidateBoundsAndTombstones(t *testing.T) {
	valid := manifest("v", "content")
	if err := valid.Validate(1); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]*Manifest{
		"negative size": {Size: -1},
		"zero block":    {Blocks: []Block{{Size: 0}}},
		"bad tombstone": {Size: 1, Tombstone: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.Validate(10); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	if err := valid.Validate(0); err == nil {
		t.Fatal("invalid maximum accepted")
	}
}
