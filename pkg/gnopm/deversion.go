package gnopm

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Deversion is the one-time migration from version-in-the-directory to
// version-in-the-module-line, for a whole workspace at once.
//
// It is a gnopm command rather than a throwaway script because every gno
// repository that adopts this convention has to run exactly this migration,
// and the ordering is the part that is easy to get wrong:
//
//  1. Freeze. Pin every package in the tree at HEAD, so every version is
//     addressable from history BEFORE anything is removed from the tree.
//     Skipping this is unrecoverable: a version deleted without a pin is a
//     version nobody can resolve again without archaeology.
//  2. Move. For each package whose directory ends in the same /vN its module
//     declares, lift its files one level up with `git mv`, so git records
//     renames and a reviewer sees a rename list rather than a rewrite.
//     Where several versions of the same package exist, the highest one wins
//     the directory and the rest leave the tree; they stay resolvable from
//     the pin written in step 1.
//  3. Re-lock. The moved packages flip back to { dir } entries at their new
//     location; the ones that left the tree keep their history pins.
//
// The module lines are not touched. A realm's address derives from its
// package path, so rewriting one here would silently change an address; the
// whole point is that the migration is a pure directory move.
//
// What it cannot do is move the code that was pointing at those directories
// from outside the workspace, so it reports it instead: see outsideRefs.
func Deversion(e *Env, opts DeversionOptions) error {
	root, w := e.Root, e.Errw
	pkgs, err := scanPackages(root)
	if err != nil {
		return err
	}
	moves, drops, skips, err := planDeversion(pkgs, opts)
	if err != nil {
		return err
	}
	if len(moves) == 0 && len(drops) == 0 {
		if len(skips) == 0 {
			fmt.Fprintln(w, "nothing to do: no package directory carries its version")
			return nil
		}
		writeSkips(w, skips)
		fmt.Fprintf(w, "\nnothing moves: %d package(s) skipped.\n", len(skips))
		return nil
	}

	if opts.DryRun {
		for _, m := range moves {
			if m.Replaces != "" {
				fmt.Fprintf(w, "up   %s -> %s   (%s, replacing the version already there)\n", m.From, m.To, m.Module)
				continue
			}
			fmt.Fprintf(w, "mv   %s -> %s   (%s)\n", m.From, m.To, m.Module)
		}
		for _, d := range drops {
			fmt.Fprintf(w, "rm   %s   (%s, superseded, kept in %s)\n", d.Dir, d.Module, lockFile)
		}
		writeSkips(w, skips)
		fmt.Fprintf(w, "\n%d package(s) move, %d leave the tree%s.\n", len(moves), len(drops), skipSuffix(skips))
		reportOutsideRefs(w, root, moves, drops)
		return nil
	}

	dirs := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		dirs = append(dirs, p.Dir)
	}
	if dirty, err := gitDirty(root, dirs...); err != nil {
		return err
	} else if len(dirty) > 0 {
		return fmt.Errorf("%d package file(s) are uncommitted (e.g. %s) - commit before migrating",
			len(dirty), strings.TrimSpace(dirty[0]))
	}

	// Step 1: freeze, so nothing below this line can lose a version.
	old, err := readLock(root)
	if err != nil {
		return err
	}
	frozen, err := freezeLock(root, old, pkgs, w)
	if err != nil {
		return err
	}
	if err := writeLock(root, frozen); err != nil {
		return err
	}
	fmt.Fprintf(w, "froze %d packages\n", len(pkgs))

	// Step 2: move, one tracked file at a time, so git sees renames.
	//
	// Collisions are checked for the whole plan first. Packages nest here
	// (p/moul/ulist/v0 and p/moul/ulist/lplist/v0 both exist), so lifting one
	// package's files up a level can in principle land on a sibling package's
	// directory. Half a migration is much worse than none, so nothing moves
	// until the whole plan is known to be collision-free.
	moveFiles := make([][]string, len(moves))
	for i, m := range moves {
		files, err := gitTrackedIn(root, m.From)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return fmt.Errorf("%s: no tracked files", m.From)
		}
		moveFiles[i] = files
	}
	if err := checkCollisions(root, moves, moveFiles); err != nil {
		return err
	}
	for i, m := range moves {
		if m.Replaces != "" {
			// Drop the outgoing version's files. Its content is already
			// pinned by the freeze above, so nothing is lost; leaving them
			// would merge two versions into one directory.
			old, err := gitTrackedIn(root, m.Replaces)
			if err != nil {
				return err
			}
			for _, f := range old {
				if _, err := git(root, "rm", "-q", "--", path.Join(m.Replaces, f)); err != nil {
					return err
				}
			}
		}
		files := moveFiles[i]
		for _, f := range files {
			src := path.Join(m.From, f)
			dst := path.Join(m.To, f)
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(path.Dir(dst))), 0o755); err != nil {
				return err
			}
			if _, err := git(root, "mv", src, dst); err != nil {
				return err
			}
		}
		// The version directory holds no tracked files now. git does not track
		// directories, so it will not clean this up: the empty tree would sit
		// there untracked and invisible to `git status`, while `ls` still
		// shows a v1/ that is supposed to be gone.
		//
		// Recursive, not a single Remove: a package with a subdirectory
		// (filetests/, say) leaves an empty subdirectory inside the empty
		// version directory, and one Remove would fail on "directory not
		// empty" and be silently ignored.
		if err := removeEmptyTree(filepath.Join(root, filepath.FromSlash(m.From))); err != nil {
			return err
		}
	}
	for _, d := range drops {
		if _, err := git(root, "rm", "-r", "-q", d.Dir); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "moved %d package(s), removed %d superseded version(s)%s\n", len(moves), len(drops), skipSuffix(skips))
	writeSkips(w, skips)

	// Step 3: re-lock against the new layout.
	pkgs, err = scanPackages(root)
	if err != nil {
		return err
	}
	relocked, err := buildLock(frozen, pkgs)
	if err != nil {
		return err
	}
	if err := writeLock(root, relocked); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: %d modules (%d in tree, %d pinned to history)\n",
		lockFile, len(relocked.Modules), len(pkgs), len(relocked.Modules)-len(pkgs))
	reportOutsideRefs(w, root, moves, drops)
	fmt.Fprintf(w, "\nnext: `make lint test`, then commit the move on its own.\n")
	return nil
}

// DeversionOptions are what a migration can be told that it cannot work out.
// The zero value is the migration, and every field exists because a real
// repository needed it: see the comment on each.
type DeversionOptions struct {
	// DryRun prints the plan and changes nothing.
	DryRun bool
	// Skip excludes a package whose directory, relative to the workspace
	// root, matches one of these path.Match globs. For the subtree a
	// repository keeps out of a migration on purpose: gnolang/gno holds a
	// versioning tutorial under examples/quarantined/ that exists
	// unversioned and under v1/ and v2/ at once, which is not a layout to
	// fix but a thing being taught.
	Skip []string
	// KeepSuperseded leaves an older version in the directory it already
	// occupies instead of taking it out of the tree.
	//
	// The default is right when consumers resolve through gnopm, and wrong
	// when the tree is itself the distribution: in gnolang/gno, genesis
	// loading, the txtar lanes, gnodev and gnoe2e all read the workspace off
	// disk, so a version that lives only in the lock is a version each of
	// them loses until something materializes it first.
	//
	// Deliberately not offered on bump: keeping the superseded version there
	// would mean copying a directory and editing the module line of the
	// copy, which is the copy git cannot pair, which is what gnopm exists to
	// remove. deversion is different because the version is already in the
	// tree and the migration is what takes it out.
	KeepSuperseded bool
}

// skip is a package the plan leaves exactly where it is, and why. A skip is
// never an error: the migration moves what it can and says what it did not.
type skip struct {
	Dir, Why string
	// Group collapses the report to one line. An excluded subtree is a
	// decision the caller already made and does not need read back: on
	// gnolang/gno, -skip quarantined/* covers 174 packages, and printing all
	// of them buries the one skip that is news.
	Group string
}

func writeSkips(w io.Writer, skips []skip) {
	counts := map[string]int{}
	var groups []string
	for _, s := range skips {
		if s.Group == "" {
			continue
		}
		if counts[s.Group] == 0 {
			groups = append(groups, s.Group)
		}
		counts[s.Group]++
	}
	sort.Strings(groups)
	for _, g := range groups {
		fmt.Fprintf(w, "skip %d package(s)   (%s)\n", counts[g], g)
	}
	for _, s := range skips {
		if s.Group == "" {
			fmt.Fprintf(w, "skip %s   (%s)\n", s.Dir, s.Why)
		}
	}
}

func skipSuffix(skips []skip) string {
	if len(skips) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d skipped", len(skips))
}

type move struct {
	From, To, Module string
	// Replaces is the package already sitting at To, when this move is an
	// upgrade rather than a migration: the branch added pkg/vN+1 as a
	// directory while pkg/ already holds vN. Its files are removed first, and
	// the version it held has already been pinned by the freeze.
	Replaces string
}
type drop struct{ Dir, Module string }

// planDeversion decides what moves where, and refuses rather than guesses.
//
// Refusing is per package, not per run: a directory that cannot be lifted is
// reported and left alone, and the rest of the tree still migrates. The whole
// plan used to fail instead, which on gnolang/gno meant one tutorial package
// in a directory the migration was not even supposed to touch stopped all 104
// moves, with no way to scope it out.
func planDeversion(pkgs []Package, opts DeversionOptions) ([]move, []drop, []skip, error) {
	// Group packages by the directory they would end up in.
	type cand struct {
		pkg Package
		n   int
	}
	byTarget := map[string][]cand{}
	atDir := map[string]Package{}
	for _, p := range pkgs {
		atDir[p.Dir] = p
	}
	var moves []move
	var drops []drop
	var skips []skip
	for _, p := range pkgs {
		if g, ok := matchAny(opts.Skip, p.Dir); ok {
			skips = append(skips, skip{Dir: p.Dir, Why: "excluded by -skip " + g, Group: "excluded by -skip " + g})
			continue
		}
		_, n, ok := splitVersion(p.Module)
		if !ok {
			// No trailing /vN in the module path: nothing to de-version, and
			// not this migration's business to invent one.
			continue
		}
		dirBase, dirN, dirVersioned := splitVersion(p.Dir)
		if !dirVersioned {
			continue // already de-versioned
		}
		if dirN != n {
			return nil, nil, nil, fmt.Errorf("%s declares %s but sits in a /v%d directory - "+
				"fix the mismatch by hand before migrating, or exclude it with -skip %s", p.Dir, p.Module, dirN, p.Dir)
		}
		byTarget[dirBase] = append(byTarget[dirBase], cand{p, n})
	}

	targets := make([]string, 0, len(byTarget))
	for t := range byTarget {
		targets = append(targets, t)
	}
	sort.Strings(targets)

	for _, target := range targets {
		cands := byTarget[target]
		// Highest version wins the unversioned directory: it is the one being
		// developed, and the one a bump will edit next.
		sort.Slice(cands, func(i, j int) bool { return cands[i].n > cands[j].n })
		m := move{From: cands[0].pkg.Dir, To: target, Module: cands[0].pkg.Module}
		// Is there already a package at the destination? That happens when a
		// branch bumps an existing package the old way, by copying it to
		// pkg/vN+1 while pkg/ still holds vN. It is an upgrade, not a
		// collision, and the author plainly meant a bump.
		if cur, ok := atDir[target]; ok {
			_, curN, curOK := splitVersion(cur.Module)
			if !curOK || curN >= cands[0].n {
				// A package that exists both unversioned and under a /vN, so
				// lifting the versioned one has nowhere to land. Which of the
				// two is current is the repository's call, not gnopm's, and
				// sometimes it is neither: the versioning tutorial in
				// gnolang/gno is unversioned and v1/ and v2/ on purpose.
				for _, c := range cands {
					skips = append(skips, skip{Dir: c.pkg.Dir, Why: fmt.Sprintf(
						"%s already declares %s, so %s has nowhere to land", target, cur.Module, c.pkg.Module)})
				}
				continue
			}
			m.Replaces = target
		}
		moves = append(moves, m)
		for _, c := range cands[1:] {
			if opts.KeepSuperseded {
				skips = append(skips, skip{Dir: c.pkg.Dir, Why: c.pkg.Module + " superseded, kept in the tree by -keep-superseded"})
				continue
			}
			drops = append(drops, drop{Dir: c.pkg.Dir, Module: c.pkg.Module})
		}
	}
	sort.Slice(moves, func(i, j int) bool { return moves[i].From < moves[j].From })
	sort.Slice(drops, func(i, j int) bool { return drops[i].Dir < drops[j].Dir })
	sort.Slice(skips, func(i, j int) bool { return skips[i].Dir < skips[j].Dir })
	return moves, drops, skips, nil
}

// matchAny reports which glob matches dir, testing the whole path and every
// parent of it: -skip 'quarantined/*' is meant to exclude the subtree, and a
// glob that only matched its direct children would be a trap.
func matchAny(globs []string, dir string) (string, bool) {
	for _, g := range globs {
		for d := dir; ; d = path.Dir(d) {
			if ok, err := path.Match(g, d); err == nil && ok {
				return g, true
			}
			if d == "." || d == "/" || !strings.Contains(d, "/") {
				break
			}
		}
	}
	return "", false
}

// checkCollisions proves that no file a move produces lands on a path that
// already exists or that another move also claims.
func checkCollisions(root string, moves []move, files [][]string) error {
	claimed := map[string]string{}
	for i, m := range moves {
		// An upgrade legitimately lands on the outgoing version's own files;
		// they are removed first. Only files it does NOT own are a conflict.
		var replaced map[string]bool
		if m.Replaces != "" {
			replaced = map[string]bool{}
			own, err := gitTrackedIn(root, m.Replaces)
			if err != nil {
				return err
			}
			for _, f := range own {
				replaced[path.Join(m.Replaces, f)] = true
			}
		}
		for _, f := range files[i] {
			dst := path.Join(m.To, f)
			if prev, dup := claimed[dst]; dup {
				return fmt.Errorf("collision: %s and %s both want to produce %s", prev, m.From, dst)
			}
			claimed[dst] = m.From
			// Anything already at the destination that is not the source
			// itself is a genuine conflict: a sibling package directory, or a
			// stray file left at the unversioned path.
			if replaced[dst] {
				continue
			}
			abs := filepath.Join(root, filepath.FromSlash(dst))
			if _, err := os.Stat(abs); err == nil {
				return fmt.Errorf("collision: %s already exists, %s cannot move there", dst, path.Join(m.From, f))
			}
		}
	}
	return nil
}

// removeEmptyTree deletes dir if nothing but empty directories is left under
// it, and does nothing at all otherwise.
//
// Deliberately conservative: it never removes a file, so a stray untracked
// file anywhere under the directory stops the whole prune rather than being
// destroyed. Somebody's uncommitted scratch file is worth more than a tidy
// tree.
func removeEmptyTree(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			return nil // a real file lives here; leave everything alone
		}
		if err := removeEmptyTree(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	// Re-read: the children may or may not have gone.
	entries, err = os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(dir)
	}
	return nil
}
