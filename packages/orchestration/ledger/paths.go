package ledger

import (
	"path"
	"sort"
	"strings"
)

// writePath is one declared write: a file, or (Prefix) a directory and everything below it.
type writePath struct {
	Path   string // repository-relative, cleaned, never with a trailing slash
	Prefix bool
}

// String is the declared form: a directory ends in "/".
func (w writePath) String() string {
	if w.Prefix {
		return w.Path + "/"
	}
	return w.Path
}

// parseWrite reads a declared write. A trailing "/" makes it a directory. The path must stay inside the
// repository: no leading "/", no "..", not the repository root itself.
func parseWrite(raw string) (writePath, error) {
	s := strings.TrimSpace(raw)
	prefix := strings.HasSuffix(s, "/")
	p, err := cleanRepoPath(s)
	if err != nil {
		return writePath{}, err
	}
	return writePath{Path: p, Prefix: prefix}, nil
}

// cleanRepoPath cleans a repository-relative path ("./a//b/" becomes "a/b").
func cleanRepoPath(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", invalid("empty path")
	}
	if strings.HasPrefix(s, "/") {
		return "", invalid("path %q is absolute; give it relative to the repository root", raw)
	}
	p := path.Clean(s)
	if p == "." {
		return "", invalid("path %q is the whole repository", raw)
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", invalid("path %q leaves the repository", raw)
	}
	return p, nil
}

// parseWrites reads a declared write set, dropping duplicates and sorting it.
func parseWrites(raw []string) ([]writePath, error) {
	seen := map[writePath]bool{}
	var out []writePath
	for _, r := range raw {
		w, err := parseWrite(r)
		if err != nil {
			return nil, err
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// under reports whether p is d or lies below it.
func under(p, d string) bool { return p == d || strings.HasPrefix(p, d+"/") }

// covers reports whether a declared write permits changing the file p.
func (w writePath) covers(p string) bool {
	if w.Prefix {
		return strings.HasPrefix(p, w.Path+"/")
	}
	return p == w.Path
}

// overlaps reports whether two declared writes could name the same file, in either direction of containment.
// A file and a directory of the same name count as overlapping: one change could replace one by the other.
func (w writePath) overlaps(o writePath) bool {
	switch {
	case w.Prefix && o.Prefix:
		return under(w.Path, o.Path) || under(o.Path, w.Path)
	case w.Prefix:
		return under(o.Path, w.Path)
	case o.Prefix:
		return under(w.Path, o.Path)
	default:
		return w.Path == o.Path
	}
}

// clashes lists every pair (ours, theirs) that overlaps.
func clashes(ours, theirs []writePath) []PathClash {
	var out []PathClash
	for _, a := range ours {
		for _, b := range theirs {
			if a.overlaps(b) {
				out = append(out, PathClash{Ours: a.String(), Theirs: b.String()})
			}
		}
	}
	return out
}
