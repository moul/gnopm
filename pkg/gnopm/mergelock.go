package gnopm

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Resolving a conflicted gnomod.lock.
//
// Squash-merging a base branch makes every stacked branch conflict on the lock,
// and taking a side loses data silently. `--ours` drops the pins the base added
// that the feature branch has no entry for at all, `gnopm sync` does not restore
// them (buildLock carries over entries the old lock already had, and these were
// never in it), and `gnopm verify` then passes, because a lock that never
// mentions a version is consistent, just poorer. Whatever imported those
// versions stops resolving later, somewhere else.
//
// The rule needs no judgement, which is the whole argument for a command:
//
//	both sides identical            either
//	only one side has the module    that side
//	{ dir } against { commit, hash} the pinned one
//
// The third row follows from what a pin means. The side that pinned a version
// is the side that bumped past it, so that version has to stay resolvable from
// history; the side still calling it { dir } has simply not caught up.

// MergeOptions is what merge-lock was asked to do.
type MergeOptions struct {
	// DryRun prints the resolution and changes nothing. It matters more here
	// than anywhere else: nobody should first meet this command mid-merge with
	// no way to look before it writes.
	DryRun bool
}

// mergeTake records one decision, for the report.
type mergeTake struct {
	module string
	side   string // "ours", "theirs"
	why    string
}

// MergeLock resolves a conflicted gnomod.lock out of the index and syncs.
func MergeLock(e *Env, opts MergeOptions) error {
	unmerged, err := git(e.Root, "ls-files", "-u", "--", lockFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(unmerged) == "" {
		// Not an error. Plenty of merges conflict only in source files, and
		// running this afterwards out of habit should cost nothing.
		e.logf("%s is not conflicted, nothing to resolve\n", lockFile)
		return nil
	}

	base, _ := stageLock(e.Root, 1)
	ours, okOurs := stageLock(e.Root, 2)
	theirs, okTheirs := stageLock(e.Root, 3)
	if !okOurs && !okTheirs {
		return fmt.Errorf("neither side of the merge has a readable %s: resolve it by hand, then `gnopm sync`", lockFile)
	}

	oursBy, err := ours.ByModule()
	if err != nil {
		return fmt.Errorf("our side: %w", err)
	}
	theirsBy, err := theirs.ByModule()
	if err != nil {
		return fmt.Errorf("their side: %w", err)
	}
	baseBy, _ := base.ByModule()

	var modules []string
	seen := map[string]bool{}
	for _, m := range []map[string]*LockEntry{oursBy, theirsBy} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				modules = append(modules, k)
			}
		}
	}
	sort.Strings(modules)

	next := &Lock{Format: lockFormat}
	var takes []mergeTake
	var refuse []string
	identical := 0
	for _, m := range modules {
		o, haveOurs := oursBy[m]
		t, haveTheirs := theirsBy[m]
		switch {
		case haveOurs && haveTheirs && sameEntry(*o, *t):
			identical++
			next.Modules = append(next.Modules, *o)
		case haveOurs && !haveTheirs:
			next.Modules = append(next.Modules, *o)
			takes = append(takes, mergeTake{m, "ours", onlySideWhy(baseBy, m, "theirs")})
		case !haveOurs && haveTheirs:
			next.Modules = append(next.Modules, *t)
			takes = append(takes, mergeTake{m, "theirs", onlySideWhy(baseBy, m, "ours")})
		case o.Source.InTree() && !t.Source.InTree():
			next.Modules = append(next.Modules, *t)
			takes = append(takes, mergeTake{m, "theirs", "pinned there, still { dir } here"})
		case !o.Source.InTree() && t.Source.InTree():
			next.Modules = append(next.Modules, *o)
			takes = append(takes, mergeTake{m, "ours", "pinned here, still { dir } there"})
		case o.Source.InTree() && t.Source.InTree():
			// Two { dir } entries pointing at different directories: the
			// package moved on one side. Either is safe, because Sync below
			// rebuilds every in-tree entry from the working tree anyway. Say
			// so rather than looking arbitrary.
			next.Modules = append(next.Modules, *o)
			takes = append(takes, mergeTake{m, "ours", fmt.Sprintf("both { dir } (%s vs %s); sync re-derives it from the tree", o.Source.Dir, t.Source.Dir)})
		default:
			// Two different pins for one module. This is the genuinely
			// ambiguous case, it is not what a squash merge produces, and
			// guessing would strand whichever version is real.
			refuse = append(refuse, fmt.Sprintf("  %s\n    ours:   %s in %s\n    theirs: %s in %s",
				m, short(o.Source.Commit), o.Source.Dir, short(t.Source.Commit), t.Source.Dir))
		}
	}
	if len(refuse) > 0 {
		return fmt.Errorf("%d module(s) are pinned to different commits on the two sides, which needs a human:\n%s\n"+
			"  a squash merge does not produce this. Resolve %s by hand, then `gnopm sync`",
			len(refuse), strings.Join(refuse, "\n"), lockFile)
	}
	next.Sort()

	// Silence is the wrong default for once: the whole failure mode this
	// replaces is a pin disappearing without anyone noticing.
	e.logf("%s: %d module(s), %d identical on both sides\n", lockFile, len(next.Modules), identical)
	for _, tk := range takes {
		e.logf("  take %-6s %s (%s)\n", tk.side, tk.module, tk.why)
	}
	if len(takes) == 0 {
		e.logf("  nothing divergent: the conflict was formatting only\n")
	}
	if opts.DryRun {
		e.logf("\n-n: nothing written. Drop it to resolve and `git add %s`\n", lockFile)
		return nil
	}

	if err := os.WriteFile(filepath.Join(e.Root, lockFile), []byte(next.String()), 0o644); err != nil {
		return err
	}
	if _, err := git(e.Root, "add", "--", lockFile); err != nil {
		return err
	}
	// Sync last, so the assembly matches the lock that was just written and
	// the merge can be committed without a second command.
	if err := Sync(e); err != nil {
		return err
	}
	e.logf("resolved and staged. `gnopm verify` proves it, then finish the merge\n")
	return nil
}

// onlySideWhy explains a module present on one side only.
//
// The base matters for exactly this: a module the base had and one side no
// longer has was dropped on purpose there, and the union brings it back. That
// is the safe direction, because a resurrected dead entry is dropped again by
// `gnopm tidy` while a lost pin is not recoverable, but it is worth saying out
// loud rather than leaving somebody to find it later.
func onlySideWhy(baseBy map[string]*LockEntry, module, missingSide string) string {
	if baseBy != nil && baseBy[module] != nil {
		return "only side with it; " + missingSide + " dropped it, `gnopm tidy` drops it again if that was meant"
	}
	return "only side with it"
}

func sameEntry(a, b LockEntry) bool {
	return a.Source == b.Source && a.Hash == b.Hash
}

// stageLock reads one merge stage out of the index. A missing stage is an empty
// lock, not an error: that is a side which deleted the file.
func stageLock(root string, stage int) (*Lock, bool) {
	out, err := git(root, "show", fmt.Sprintf(":%d:%s", stage, lockFile))
	if err != nil {
		return &Lock{Format: lockFormat}, false
	}
	l, err := parseLock(out)
	if err != nil {
		return &Lock{Format: lockFormat}, false
	}
	return l, true
}

func cmdMergeLock(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("merge-lock takes no arguments, got %q", args[0])
	}
	return MergeLock(e, MergeOptions{DryRun: flagBool(fs, "n")})
}
