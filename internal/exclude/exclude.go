// Package exclude compiles exclusion globs once, before filesystem traversal.
package exclude

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

type rule struct {
	re  *regexp.Regexp
	dir bool
}
type Matcher struct{ rules []rule }

// Compile supports *, ?, character classes and ** as a whole path component.
// A leading slash anchors at the root; slash-free patterns match any component.
// Negation is intentionally rejected: excluded directories are never traversed.
func Compile(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	for _, pattern := range append([]string{".nas-sync/"}, patterns...) {
		if pattern == "" || strings.HasPrefix(pattern, "!") || strings.Contains(pattern, "\\") {
			return nil, fmt.Errorf("unsupported exclusion %q", pattern)
		}
		dir := strings.HasSuffix(pattern, "/")
		anchor := strings.HasPrefix(pattern, "/")
		p := strings.Trim(pattern, "/")
		if p == "" {
			return nil, fmt.Errorf("empty exclusion")
		}
		var b strings.Builder
		if anchor || strings.Contains(p, "/") {
			b.WriteString("^")
		} else {
			b.WriteString("(^|/)")
		}
		parts := strings.Split(p, "/")
		for i, part := range parts {
			if part == "." || part == ".." || part == "" {
				return nil, fmt.Errorf("invalid exclusion %q", pattern)
			}
			if part == "**" {
				if i == len(parts)-1 {
					b.WriteString(".*")
				} else {
					b.WriteString("(?:[^/]+/)*")
				}
				continue
			}
			if strings.Contains(part, "**") {
				return nil, fmt.Errorf("** must be a whole component in %q", pattern)
			}
			if _, err := path.Match(part, ""); err != nil {
				return nil, err
			}
			for j := 0; j < len(part); j++ {
				switch part[j] {
				case '*':
					b.WriteString("[^/]*")
				case '?':
					b.WriteString("[^/]")
				case '[':
					k := j + 1
					if k < len(part) && part[k] == '^' {
						k++
					}
					for k < len(part) && part[k] != ']' {
						k++
					}
					if k == len(part) {
						return nil, fmt.Errorf("invalid class in %q", pattern)
					}
					b.WriteString(part[j : k+1])
					j = k
				default:
					b.WriteString(regexp.QuoteMeta(string(part[j])))
				}
			}
			if i < len(parts)-1 {
				b.WriteString("/")
			}
		}
		b.WriteString("$")
		re, err := regexp.Compile(b.String())
		if err != nil {
			return nil, err
		}
		m.rules = append(m.rules, rule{re, dir})
	}
	return m, nil
}

// Match also checks ancestors, so a removed directory's descendants remain excluded.
func (m *Matcher) Match(rel string, isDir bool) bool {
	if rel == "." {
		return false
	}
	for {
		for _, r := range m.rules {
			if (!r.dir || isDir) && r.re.MatchString(rel) {
				return true
			}
		}
		i := strings.LastIndexByte(rel, '/')
		if i < 0 {
			return false
		}
		rel = rel[:i]
		isDir = true
	}
}
