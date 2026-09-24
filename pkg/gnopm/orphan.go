package gnopm

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// An orphan is a lock entry that says a version lives in the working tree, in a
// directory that no longer declares it.
//
// buildLock's rule 2 carries over every old entry the tree no longer holds,
// which is right for a pinned one (that IS the archive) and wrong for a
// { dir } one: the directory is still named, but it holds somebody else's code
// now, or no code at all. sync used to write that lock and report success, and
// `consistent` (which verify and status share) then called it stale forever.
//
// Two ways to get one, and both are a human editing the tree directly rather
// than going through `bump`, which pins the outgoing version before it rewrites
// anything:
//
//	sed -i 's|/md/v0|/md/v1|' p/alice/md/gnomod.toml   # the module line moved
//	rm -rf p/alice/set                                  # the directory is gone
//
// The answer is the one `bump` would have given: the version's source is still
// in git, so pin it there. That is what repinOrphans does, and it is why `sync`
// converges.

// orphan is one such entry, with what its directory declares now.
type orphan struct {
	Entry LockEntry
	// Now is the module that directory declares today, empty when the
	// directory is gone. It only shapes the error, not the repair.
	Now string
}

// findOrphans returns the entries of next whose directory no longer declares
// them. Nil in the overwhelmingly common case, which is what keeps sync free
// of git when there is nothing to do.
func findOrphans(next *Lock, pkgs []Package) []orphan {
	byDir := make(map[string]string, len(pkgs))
	inTree := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		byDir[p.Dir] = p.Module
		inTree[p.Module] = true
	}
	var out []orphan
	for _, e := range next.Modules {
		if !e.Source.InTree() || inTree[e.Module] {
			continue
		}
		out = append(out, orphan{Entry: e, Now: byDir[e.Source.Dir]})
	}
	return out
}

// repinOrphans rewrites every orphaned entry to the commit that still holds its
// source, in place in next.
//
// It refuses rather than guessing when a version exists on no commit at all,
// which is the one case git cannot answer: a package added and taken away again
// without ever being committed was never anywhere, so there is nothing to pin
// and nothing that could ever have imported it from history.
func repinOrphans(root string, next *Lock, pkgs []Package, w io.Writer) error {
	orphans := findOrphans(next, pkgs)
	if len(orphans) == 0 {
		return nil
	}
	byModule := make(map[string]int, len(next.Modules))
	for i, e := range next.Modules {
		byModule[e.Module] = i
	}
	for _, o := range orphans {
		dir, module := o.Entry.Source.Dir, o.Entry.Module
		target, err := pinOrphan(root, dir, module, w)
		if err != nil {
			return orphanRefusal(o, err)
		}
		i := byModule[module]
		next.Modules[i].Source = Source{Commit: target.Commit, Dir: dir}
		next.Modules[i].Hash = target.Hash
		if w != nil {
			where := "the directory is gone"
			if o.Now != "" {
				where = dir + " declares " + o.Now + " now"
			}
			ref := ""
			if target.Ref != "" {
				ref = " (" + target.Ref + ")"
			}
			fmt.Fprintf(w, "%s: %s, pinned to %s%s\n", module, where, short(target.Commit), ref)
		}
	}
	return nil
}

// orphanRefusal is the message for the case nothing can repair.
//
// It names both recoveries rather than one, because which is right depends on
// something gnopm cannot know: whether that version should still resolve. The
// error a user sees has to be actionable without reading the source.
func orphanRefusal(o orphan, cause error) error {
	dir, module := o.Entry.Source.Dir, o.Entry.Module
	what := fmt.Sprintf("%s is locked at %s, which is gone", module, dir)
	if o.Now != "" {
		what = fmt.Sprintf("%s is locked at %s, which now declares %s", module, dir, o.Now)
	}
	return fmt.Errorf("%s, and %w.\n"+
		"  Nothing records where that version's code is, so anything importing it stops resolving.\n"+
		"  Restore it and run `gnopm bump %s`, which pins the outgoing version first; "+
		"or delete its [[module]] block from %s if that version should stop resolving",
		what, cause, dir, lockFile)
}

// maxPinSearch caps how far back a pin is looked for.
//
// The log is already limited to one directory, so this is reached only by a
// package with hundreds of commits of its own, where a version that old is not
// the one being taken out of the tree today.
const maxPinSearch = 500

// pinOrphan finds the newest commit whose <dir>/gnomod.toml declared module,
// and hashes the directory there.
//
// The default branch is searched first and is not merely a preference: this
// repository, and most, squash-merge, so a pin to a commit that exists only on
// a feature branch dangles the moment the pull request lands. `gnopm verify
// -upstream` rejects exactly that, so producing one here would mean gnopm
// writing a lock its own guard fails. Falling back to HEAD's history is the
// honest answer when the version genuinely exists nowhere else, and it warns.
func pinOrphan(root, dir, module string, w io.Writer) (pinTarget, error) {
	if ref := pinBaseRef(root); ref != "" {
		if base, err := gitResolve(root, ref); err == nil {
			if t, ok := searchHistory(root, base, dir, module); ok {
				t.Ref = ref
				return t, nil
			}
		}
	}
	t, ok := searchHistory(root, "HEAD", dir, module)
	if !ok {
		return pinTarget{}, fmt.Errorf("no commit reachable from HEAD holds %s at %s", module, dir)
	}
	if w != nil {
		fmt.Fprintf(w, "warning: %s is pinned to %s, which is not on the default branch.\n"+
			"  This repository squash-merges, so that commit will not survive the merge.\n"+
			"  Re-run `gnopm sync` after the change lands, or `gnopm verify -upstream` will reject it.\n",
			module, short(t.Commit))
	}
	return t, nil
}

// searchHistory walks rev's commits that touched dir, newest first, and returns
// the first one whose gnomod.toml declares module.
//
// The log is over the whole directory rather than over gnomod.toml alone, and
// that is the point: the newest commit declaring a version holds that version's
// final content, which is what has to be pinned. Logging only the manifest
// would find the commit that introduced the module line and miss every change
// to the code made under it afterwards.
func searchHistory(root, rev, dir, module string) (pinTarget, bool) {
	out, err := git(root, "log", "--format=%H", "-n", strconv.Itoa(maxPinSearch), rev, "--", dir)
	if err != nil {
		return pinTarget{}, false
	}
	for _, commit := range strings.Fields(out) {
		// A commit that removed the directory has no manifest to read, and a
		// commit from before it existed has none either. Both are a skip
		// rather than a failure: the log covers a directory's whole life.
		b, err := git(root, "show", commit+":"+dir+"/"+gnomodFile)
		if err != nil {
			continue
		}
		declared, ignored, err := parseGnomod([]byte(b), dir+"/"+gnomodFile)
		if err != nil || ignored || declared != module {
			continue
		}
		h, err := hashAtCommit(root, commit, dir)
		if err != nil {
			continue
		}
		return pinTarget{Commit: commit, Hash: h}, true
	}
	return pinTarget{}, false
}
