package gnopm

import (
	"fmt"
	"strings"
)

// A shallow clone cannot answer the questions the history checks ask, and git
// does not say so: it answers "no".
//
// `git merge-base --is-ancestor` returns non-zero for a commit the repository
// does not have, which is the same answer it gives for a commit that genuinely
// is not upstream. On a complete repository those two cases cannot be
// confused. On a shallow one every pin older than the boundary takes the first
// branch and is reported as the second.
//
// That is not a hypothetical. A workflow in moul/gno-contracts checked out at
// fetch-depth: 0 and then ran `git fetch origin main --depth=100` a few lines
// later, which does not deepen a complete repository: it grafts a boundary on
// and discards the rest. `gnopm tool ci` then reported that a squash merge
// would strand 39 of 42 pinned versions, with three green checks around it and
// nothing actually wrong (moul/gnopm#72, fixed in moul/gno-contracts#226).
//
// A wrong answer delivered confidently is worse than no answer, and the advice
// attached to it was worse still: "Bump before editing, or `gnopm tidy`", where
// tidy on that reading would drop the very pins it could not see.

// beyondTheBoundary names the pinned modules whose commits this repository
// cannot reach, when and only when it is shallow.
//
// Gated on shallowness deliberately. A complete repository missing a pinned
// commit is a real finding, and `reproduces` is right to report it as one.
func beyondTheBoundary(root string, pinned []LockEntry) []string {
	if !gitIsShallow(root) {
		return nil
	}
	var missing []string
	for _, en := range pinned {
		// A { chain } entry has no commit for git to find, so it is neither
		// reachable nor unreachable here.
		if en.Source.Commit == "" {
			continue
		}
		if _, err := gitResolve(root, en.Source.Commit); err != nil {
			missing = append(missing, en.Module)
		}
	}
	return missing
}

// errBeyondTheBoundary is what every history check says instead of guessing.
//
// The FIRST line carries the cause and the fix, because that is the only line
// that survives: the CI report puts one line per check in a table cell, and an
// error whose fix is on line two arrives in a job summary with the fix cut off.
// The module list and the trap that produced it follow, for the places that
// print the whole thing.
func errBeyondTheBoundary(what string, missing []string) error {
	return fmt.Errorf("shallow clone: %d pinned commit(s) are beyond its boundary, so %s cannot be "+
		"answered here. CI needs `fetch-depth: 0`\n"+
		"  unreachable: %s\n"+
		"  a later `git fetch --depth=N` re-shallows a complete repository rather than deepening it, "+
		"which looks right in a diff and is not",
		len(missing), what, strings.Join(missing, ", "))
}
