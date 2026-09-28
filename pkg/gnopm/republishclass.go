package gnopm

import "strings"

// Which differences are worth wiping a realm's state for.
//
// A redeploy re-runs init(), so every value the realm accumulated since it went
// up is gone. That price is obviously worth paying for a code change and
// obviously not worth paying for a typo in a README, and until this file
// existed -republish could not tell the two apart.
//
// It matters because of what travels. `gnokey maketx addpkg` uploads with
// MPUserAll, so every .md in the package directory goes on chain. On
// gno.land/r/moul/home the README is 15,742 of 41,768 bytes, 38% of the
// payload, and it is edited far more often than the code beside it. So the
// common case was: correct a sentence of prose, and the realm reads as
// out of date against its own repo until somebody wipes its eight slots.
//
// The signal became noisy in the one place it must not be: a real code change
// and a documentation fix looked identical in the report.
//
// The rule is deliberately the narrowest one that fixes that, because this is a
// judgement encoded in a tool and a tool that is clever here is a tool that one
// day declines to ship something that mattered:
//
//	documentation == a file whose name ends in .md, and nothing else.
//
// Not "files that cannot change behaviour", which is a bigger and more tempting
// claim. Test files are the case that tempts: gnopm's own Imports() says the VM
// never runs them, so a test-only change provably cannot alter what the realm
// does. They are still counted as substantive here, because "provably inert" is
// an argument about the VM that this file would be silently relying on, and the
// cost of being wrong is refusing to publish a change the author wanted. A .md
// is not code by inspection of its name; that is the whole of the reasoning,
// and it needs no other.

// docExt is the one extension treated as documentation.
const docExt = ".md"

// diffName strips the annotation diffSource adds, so "README.md (new)" is
// classified by its name rather than by the fact that it is new.
func diffName(entry string) string {
	for _, suffix := range []string{" (new)", " (removed)"} {
		if trimmed, ok := strings.CutSuffix(entry, suffix); ok {
			return trimmed
		}
	}
	return entry
}

// docsOnly reports whether every difference is a documentation file.
//
// False for an empty list: "nothing changed" is a different answer with a
// different message, and collapsing the two would report a package with no
// differences as one whose differences are not worth shipping.
func docsOnly(changed []string) bool {
	if len(changed) == 0 {
		return false
	}
	for _, entry := range changed {
		if !strings.HasSuffix(diffName(entry), docExt) {
			return false
		}
	}
	return true
}
