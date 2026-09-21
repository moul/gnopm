package gnopm

import (
	"fmt"
	"sort"
	"strings"
)

// lockChanges summarizes what a branch does to gnomod.lock, for a reader who
// is not going to read a lock diff.
//
// Everything comes from comparing the lock at the base ref with the lock at
// HEAD, plus one ancestry question. It deliberately does not re-derive whether
// the lock matches the working tree: the checks above own that, and a second
// implementation of the same rule eventually disagrees with the first.
func lockChanges(root, base string) []string {
	headRaw, err := git(root, "show", "HEAD:"+lockFile)
	if err != nil || strings.TrimSpace(headRaw) == "" {
		return nil
	}
	head, err := parseLock(headRaw)
	if err != nil {
		return []string{fmt.Sprintf("⚠️ `%s` at HEAD does not parse: %v", lockFile, err)}
	}
	prevBy := map[string]LockEntry{}
	if raw, err := git(root, "show", base+":"+lockFile); err == nil && strings.TrimSpace(raw) != "" {
		if p, err := parseLock(raw); err == nil {
			for _, e := range p.Modules {
				prevBy[e.Module] = e
			}
		}
	}
	headBy := map[string]LockEntry{}
	for _, e := range head.Modules {
		headBy[e.Module] = e
	}

	// A bump appears twice, as the new version arriving and the old one losing
	// its directory. Report it once, as the bump.
	covered := map[string]bool{}
	for _, e := range head.Modules {
		if _, existed := prevBy[e.Module]; existed || !e.Source.InTree() {
			continue
		}
		if from, ok := replacedBy(e.Module, headBy, prevBy); ok {
			covered[from] = true
		}
	}

	var lines []string
	for _, e := range head.Modules {
		if covered[e.Module] {
			continue
		}
		p, existed := prevBy[e.Module]
		switch {
		case !existed && e.Source.InTree():
			if from, ok := replacedBy(e.Module, headBy, prevBy); ok {
				lines = append(lines, fmt.Sprintf("⬆️ `%s` bumped to **%s**, and `%s` is now pinned to history",
					unversionedPath(e.Module), lastSegment(e.Module), lastSegment(from)))
			} else {
				lines = append(lines, fmt.Sprintf("🆕 `%s` added", e.Module))
			}
		case !existed:
			lines = append(lines, fmt.Sprintf("🧊 `%s` added, pinned to history", e.Module))
		case p.Source.InTree() && !e.Source.InTree():
			lines = append(lines, fmt.Sprintf("🧊 `%s` no longer has a directory, pinned to `%s`",
				e.Module, short(e.Source.Commit)))
		case !p.Source.InTree() && e.Source.InTree():
			lines = append(lines, fmt.Sprintf("📂 `%s` is back in the working tree at `%s`", e.Module, e.Source.Dir))
		case e.Source.Commit != "" && p.Source.Commit != e.Source.Commit:
			lines = append(lines, fmt.Sprintf("🔁 `%s` re-pinned to `%s`", e.Module, short(e.Source.Commit)))
		}
	}
	for m := range prevBy {
		if _, ok := headBy[m]; !ok {
			lines = append(lines, fmt.Sprintf("🗑️ `%s` dropped from the lock", m))
		}
	}
	sort.Strings(lines)
	return lines
}

// replacedBy finds the version this one superseded: same unversioned path, in
// the tree at the base, pinned to history now.
func replacedBy(module string, head, prev map[string]LockEntry) (string, bool) {
	base := unversionedPath(module)
	best := ""
	for m, p := range prev {
		if m == module || unversionedPath(m) != base || !p.Source.InTree() {
			continue
		}
		if h, ok := head[m]; ok && !h.Source.InTree() && m > best {
			best = m
		}
	}
	return best, best != ""
}

func unversionedPath(module string) string {
	if i := strings.LastIndex(module, "/"); i >= 0 {
		return module[:i]
	}
	return module
}

func lastSegment(module string) string {
	if i := strings.LastIndex(module, "/"); i >= 0 {
		return module[i+1:]
	}
	return module
}
