package gnopm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The publish report: what the run will do, in as few lines as it can be said.
//
// The old shape spent three to five lines per package and a paragraph per
// section, so a real workspace printed sixty lines of which about eight
// carried a decision. Everything explanatory moved behind -v, because a
// sentence explaining what a dependency layer is earns its place the first
// time and is noise the two hundredth. What is left is one line per package
// and one block per transaction, with colour doing the work the prose used to.
//
// The rule for what stays: a line survives by default if it changes what the
// reader does next. "parked, waiting on an approver" does. "a layer is
// packages that do not import each other" does not.

// Glyphs. ASCII on purpose: the listing is a column layout, and a glyph whose
// width a terminal disagrees about (East Asian Ambiguous covers most of the
// pretty ones) breaks the alignment that makes the column readable at all.
const (
	glyphNew       = "+" // absent, and this run sends it
	glyphRepublish = "*" // live, and -republish sends it again
	glyphParked    = "~" // sent, waiting on an approver
	glyphBlocked   = "!" // a dependency is not live and this run cannot fix it
	glyphLive      = "·" // nothing to do
)

// pubLine is one rendered package row.
type pubLine struct {
	glyph  string
	color  string
	path   string
	size   string
	tags   []string // trailing annotations, the reasons this row is not plain
	detail []string // sub-lines, indented under the row
}

// reportPlans prints the package listing and returns the counts the caller
// needs to decide what happens next.
//
// txOf maps a module to the 1-based transaction it lands in, and is nil when
// that is not known yet (the command-list form, or a run that is blocked). A
// single transaction annotates nothing: the column would repeat "tx1" on every
// row and say nothing.
func reportPlans(e *Env, plans []plan, domain string, txOf map[string]int, txCount int) (todo, blocked, live int) {
	var lines []pubLine
	for _, pl := range plans {
		mod := trimDomain(pl.pkg.Module, domain)
		l := pubLine{glyph: glyphLive, path: mod, size: humanBytes(int64(pl.bytes))}
		switch {
		case len(pl.missing) > 0:
			blocked++
			l.glyph, l.color = glyphBlocked, ansiRed
			for _, m := range pl.missing {
				l.detail = append(l.detail, fmt.Sprintf("blocked by %s: %s", trimDomain(m.module, domain), m.why))
			}
		case pl.republish:
			todo++
			l.glyph, l.color = glyphRepublish, ansiYellow
			l.tags = append(l.tags, fmt.Sprintf("replaces live, RESETS realm state; %d file(s) differ: %s",
				len(pl.changed), strings.Join(pl.changed, ", ")))
		case pl.state == StateAbsent:
			todo++
			l.glyph, l.color = glyphNew, ansiGreen
		case pl.state == StateParked:
			l.glyph, l.color = glyphParked, ansiYellow
			l.tags = append(l.tags, "parked, waiting on an approver")
		case pl.skipped != "":
			l.color = ansiDim
			l.tags = append(l.tags, "not republished: "+pl.skipped)
			if len(pl.changed) > 0 {
				l.detail = append(l.detail, fmt.Sprintf("%d file(s) differ: %s",
					len(pl.changed), strings.Join(pl.changed, ", ")))
			}
		default:
			live++
			l.color = ansiDim
			// Nothing to do and nothing to decide. Counted, not printed,
			// unless -v was asked for.
			if !e.Verbose {
				continue
			}
		}
		if pl.dep {
			// One word, on the row, because the header line already said how
			// many were pulled in and this is the answer to "which ones".
			l.tags = append(l.tags, "(dependency)")
		}
		if e.Verbose {
			d := fmt.Sprintf("%d bytes in %d file(s)", pl.bytes, len(pl.files))
			if len(pl.files) > 0 {
				d += fmt.Sprintf(", largest %s at %d", pl.files[0].Name, pl.files[0].Size)
			}
			l.detail = append(l.detail, d)
		}
		if n := txOf[pl.pkg.Module]; n > 0 && txCount > 1 {
			l.tags = append([]string{fmt.Sprintf("tx%d", n)}, l.tags...)
		}
		lines = append(lines, l)
	}

	if len(lines) > 0 {
		// The domain every path shares, said once, so no row spends nine
		// characters restating it. Printed only when it is doing that work.
		if domain != "" {
			e.logf("\n%s\n", e.dim(domain+"/"))
		} else {
			e.logf("\n")
		}
		wp, ws := 0, 0
		for _, l := range lines {
			wp, ws = max(wp, len([]rune(l.path))), max(ws, len([]rune(l.size)))
		}
		for _, l := range lines {
			row := fmt.Sprintf("  %s %s  %s", l.glyph, pad(l.path, wp), padLeft(l.size, ws))
			e.logf("%s", e.paint(l.color, row))
			if len(l.tags) > 0 {
				e.logf("  %s", e.paint(l.color, strings.Join(l.tags, "  ")))
			}
			e.logf("\n")
			for _, d := range l.detail {
				e.logf("%s\n", e.dim("      "+d))
			}
		}
	}
	if live > 0 && !e.Verbose {
		if len(lines) == 0 {
			e.logf("\n")
		}
		e.logf("%s\n", e.dim(fmt.Sprintf("  %s %d live, nothing to do (-v lists them)", glyphLive, live)))
	}
	return todo, blocked, live
}

// trimDomain drops the domain element a gno path starts with, which is the
// same one on every row of a given run. Left alone when the path does not
// start with it, because a path from another domain is exactly the one worth
// seeing in full.
func trimDomain(module, domain string) string {
	if domain == "" {
		return module
	}
	return strings.TrimPrefix(module, domain+"/")
}

// tildePath shortens a path under the home directory, because these are all
// under ~/.gnopm and the prefix is the least interesting part of a line whose
// point is which numbered document is which.
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel := strings.TrimPrefix(p, home+string(filepath.Separator)); rel != p {
		return "~" + string(filepath.Separator) + rel
	}
	return p
}
