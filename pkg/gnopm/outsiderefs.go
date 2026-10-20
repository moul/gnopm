package gnopm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// refsCap bounds each list. A migration on a large repository can name a
// hundred files, and a hundred-line wall at the end of a plan is a list nobody
// reads; the count still says how big the job is.
const refsCap = 20

// outsideRefs answers the question a migration raises and cannot answer from
// inside the workspace: who out there was pointing at the directories that
// just moved.
//
// Two searches, because the breakage comes in two shapes and only one of them
// is findable by searching for the old path:
//
//   - literal, a file that spells the directory out. A script passing
//     -pkgdir, a doc, a test fixture.
//   - composed, code that joins the workspace directory name with an import
//     path at run time. The old directory never appears in that source, so no
//     search for it can find it, and it is the shape that broke four CI lanes
//     in gnolang/gno#6356. The list is a heuristic and says so: most entries
//     are fine, and the reader is the only one who can tell which.
//
// Both are skipped when the workspace is the whole repository, where there is
// no outside, and both are best-effort: a migration must not fail because a
// search did.
func outsideRefs(root string, dirs []string) (literal, composed []string, workspace string) {
	top, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, ""
	}
	// Through the symlinks both ways before comparing: git answers with the
	// resolved path, and on macOS a temporary directory reaches the caller as
	// /var/... while git calls the same directory /private/var/..., which
	// would make every workspace look like it is outside its own repository.
	gitRoot := resolved(strings.TrimSpace(top))
	rel, err := filepath.Rel(gitRoot, resolved(root))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, nil, ""
	}
	workspace = filepath.ToSlash(rel)

	args := []string{"grep", "--no-color", "-l", "-F"}
	for _, d := range dirs {
		args = append(args, "-e", path.Join(workspace, d))
	}
	outside := ":!" + workspace
	literal = gitGrepFiles(gitRoot, append(args, "--", outside))

	// The directory name as a quoted string: `"examples"` in Go, and in most
	// other languages that would join it with something.
	composed = gitGrepFiles(gitRoot, []string{
		"grep", "--no-color", "-l", "-F", "-e", `"` + path.Base(workspace) + `"`, "--", outside,
	})
	return literal, composed, workspace
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// gitGrepFiles runs git grep and returns the files it names. git grep exits 1
// when nothing matches, which is an answer and not a failure, and any other
// failure returns nothing: this feeds a report, never a decision.
func gitGrepFiles(gitRoot string, args []string) []string {
	cmd := exec.Command("git", args...)
	cmd.Dir = gitRoot
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return nil
		}
	}
	files := strings.Split(strings.TrimSpace(out.String()), "\n")
	var res []string
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" {
			res = append(res, f)
		}
	}
	sort.Strings(res)
	return res
}

// reportOutsideRefs writes what the move may have broken outside the
// workspace. Silent when there is nothing, like everything else here.
func reportOutsideRefs(w io.Writer, root string, moves []move, drops []drop) {
	dirs := make([]string, 0, len(moves)+len(drops))
	for _, m := range moves {
		dirs = append(dirs, m.From)
	}
	for _, d := range drops {
		dirs = append(dirs, d.Dir)
	}
	if len(dirs) == 0 {
		return
	}
	literal, composed, workspace := outsideRefs(root, dirs)
	if len(literal) == 0 && len(composed) == 0 {
		return
	}
	fmt.Fprintf(w, "\noutside %s/, which this migration does not touch:\n", workspace)
	if len(literal) > 0 {
		fmt.Fprintf(w, "\n  %d file(s) name a directory that moves:\n", len(literal))
		writeCapped(w, literal)
	}
	if len(composed) > 0 {
		fmt.Fprintf(w, "\n  %d file(s) build a path from %q, and a path built that way\n"+
			"  now points at a directory that is gone. Resolve by the module line:\n", len(composed), path.Base(workspace))
		writeCapped(w, composed)
	}
}

func writeCapped(w io.Writer, files []string) {
	for i, f := range files {
		if i == refsCap {
			fmt.Fprintf(w, "    ... and %d more\n", len(files)-refsCap)
			break
		}
		fmt.Fprintf(w, "    %s\n", f)
	}
}
