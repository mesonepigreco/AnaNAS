package exclude

import "testing"

func TestExclusions(t *testing.T) {
	m, err := Compile([]string{"node_modules/", "*.tmp", "/only-root", "src/**/cache/", "[ab].log"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p         string
		dir, want bool
	}{
		{"node_modules", true, true}, {"x/node_modules/a.js", false, true}, {"node_modules", false, false},
		{"x/a.tmp", false, true}, {"only-root", false, true}, {"x/only-root", false, false},
		{"src/cache/a", false, true}, {"src/a/b/cache/a", false, true}, {"src/a/b/keep", false, false},
		{".nas-sync/chunks/a", false, true}, {"x/.nas-sync", true, true}, {"a.log", false, true}, {"c.log", false, false},
	} {
		if got := m.Match(tc.p, tc.dir); got != tc.want {
			t.Errorf("%s: %v", tc.p, got)
		}
	}
}
func TestInvalidPatterns(t *testing.T) {
	for _, p := range []string{"!keep", "../x", "a/**b", "[", "", "/"} {
		if _, err := Compile([]string{p}); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
}
