package gnopm

import (
	"flag"
	"fmt"
	"sort"
	"text/tabwriter"
)

// Which of this workspace's imports have a newer version on the chain.
//
// `npm outdated` and `cargo outdated` need a registry API, a semver parser and
// a policy about what "newer" means under a caret range. Here a version is the
// last element of the import path and versions are consecutive integers, so the
// whole question is: does .../v(n+1) exist on the chain.
//
// That is one query per candidate against machinery that already exists, and
// the answer is exact rather than a best guess, because a published path can
// never be redefined or withdrawn.
//
// The other half is what makes it honest: gnopm will not change anything as a
// result. A newer version is a different import path, so moving to it is
// editing the import, and no tool can do that safely on your behalf. This
// reports; it does not update.

// upgrade is one module with a newer version available.
type upgrade struct {
	Module string `json:"module"`
	// Latest is the highest consecutive version found on the chain.
	Latest string `json:"latest"`
	// Ahead is how many versions have been published past yours.
	Ahead int `json:"ahead"`
	// Chain is which chain answered.
	Chain string `json:"chain"`
}

// outdatedGap caps how far past a version to probe before giving up.
//
// bump moves one at a time and a chain cannot have a hole it is possible to
// publish through, but a version SKIPPED by a stacked pull request leaves a
// genuine hole: the README documents exactly that failure, v0 to v2 with v1
// existing nowhere. Probing only the next one would stop at that hole and
// report nothing outdated, which is wrong in the direction that matters.
const outdatedGap = 3

// Outdated reports which imported versions have a successor on chain.
func Outdated(e *Env, rpc, chainID string) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	if len(lock.Modules) == 0 {
		e.logf("no modules yet\n")
		return nil
	}

	// Candidates are every module in the lock that carries a version, plus the
	// next few numbers after each. Probing the whole set in one batch is what
	// makes this a few seconds rather than a minute: the probe already
	// concurrently reads and caches.
	type want struct {
		base string
		from int
	}
	bases := map[string]want{}
	var probe []string
	for _, en := range lock.Modules {
		base, n, ok := splitVersion(en.Module)
		if !ok {
			// An unversioned module is legal and has no successor to look
			// for. r/moul/home is the standing example.
			continue
		}
		if cur, seen := bases[base]; seen && cur.from >= n {
			continue
		}
		bases[base] = want{base: base, from: n}
	}
	for _, w := range bases {
		for i := 1; i <= outdatedGap; i++ {
			probe = append(probe, fmt.Sprintf("%s/v%d", w.base, w.from+i))
		}
	}
	if len(probe) == 0 {
		e.logf("nothing here carries a version, so nothing can be outdated\n")
		return nil
	}
	sort.Strings(probe)

	p, err := NewProbe(e, lock.Modules[0].Module, rpc, chainID)
	if err != nil {
		return err
	}
	bar := newProgress(e, "checking "+p.Chain().ID)
	err = p.Warm(probe, bar.step)
	bar.stop()
	if err != nil {
		return err
	}

	var out []upgrade
	for _, w := range bases {
		latest, ahead := w.from, 0
		for i := 1; i <= outdatedGap; i++ {
			cand := fmt.Sprintf("%s/v%d", w.base, w.from+i)
			s, err := p.State(cand)
			if err != nil {
				return err
			}
			if s == StateAbsent {
				continue
			}
			// Keep looking past a hole rather than stopping at it: a version
			// number skipped by a stacked pull request is a documented way to
			// end up with v0 and v2 and no v1.
			latest, ahead = w.from+i, ahead+1
		}
		if ahead == 0 {
			continue
		}
		out = append(out, upgrade{
			Module: fmt.Sprintf("%s/v%d", w.base, w.from),
			Latest: fmt.Sprintf("%s/v%d", w.base, latest),
			Ahead:  ahead,
			Chain:  p.Chain().ID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })

	if e.JSON {
		if out == nil {
			out = []upgrade{}
		}
		return e.writeJSON(out)
	}
	if e.Format != "" {
		vals := make([]any, len(out))
		for i, u := range out {
			vals[i] = u
		}
		return e.emit(vals...)
	}
	if len(out) == 0 {
		// Nothing on stdout: an empty answer has to pipe as empty.
		e.logf("everything here is the newest version %s has\n", p.Chain().ID)
		return nil
	}
	if e.Quiet {
		for _, u := range out {
			e.printf("%s\n", u.Module)
		}
		return nil
	}
	w := tabwriter.NewWriter(e.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODULE\tLATEST\tAHEAD")
	for _, u := range out {
		fmt.Fprintf(w, "%s\t%s\t%d\n", u.Module, u.Latest, u.Ahead)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// The advice is the point. There is no `gnopm update` and there should not
	// be: a newer version is a different import path, so moving to it is
	// editing the import, and doing that on somebody's behalf is a refactor
	// rather than a package operation.
	e.logf("\nmoving to a newer version means editing the import: there is no range to\n" +
		"  re-solve and nothing moves under you. `gnopm doc <module>` on both versions\n" +
		"  shows what changed between them.\n")
	return nil
}

func cmdOutdated(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("outdated takes no arguments: it checks every versioned module in %s", lockFile)
	}
	return Outdated(e, flagString(fs, "rpc"), flagString(fs, "chainid"))
}
