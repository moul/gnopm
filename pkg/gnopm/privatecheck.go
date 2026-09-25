package gnopm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Does the chain's copy of a package still agree with the repo about `private`.
//
// `private = true` is the difference between a realm its creator can replace
// at the same path and one frozen at the bytes it first shipped with. The flag
// is read off the mempackage the transaction carries, once, and nothing reads
// it again: a live public package refuses a private redeploy as "package
// already exists", and the reverse is refused outright as "a private package
// cannot be overridden by a public package". So the moment a package goes up,
// that line in the repo stops being a setting and becomes a claim about
// something already decided, and a wrong claim has no expiry.
//
// gno.land/r/moul/faucet/v0 is what that costs when nothing compares them. Its
// deploy landed two minutes before the commit that added `private = true`, so
// the repo has advertised a redeployable realm ever since and the chain has
// never held one. Every tool involved worked correctly. None of them looked.
//
// publish is where the comparison belongs because publish is the one command
// already holding both halves: it reads the workspace to build a payload and
// the chain to decide what is absent. The extra cost is one vm/qfile per
// package that is already live, and live packages are exactly the ones it
// otherwise does nothing with.

// privateMismatch is one package whose declared `private` disagrees with the
// copy the chain holds at the same path.
type privateMismatch struct {
	module  string
	inRepo  bool
	onChain bool
}

// why states the disagreement in the direction that matters to the reader: what
// the repo promises, and what they actually have.
func (m privateMismatch) why() string {
	if m.inRepo {
		return "the repo declares private = true, the chain's copy does not: " +
			"this package is frozen public and can never be redeployed"
	}
	return "the chain's copy declares private = true, the repo does not: " +
		"a redeploy is possible and the repo does not say so"
}

// chainPrivate reports whether the copy of module that c holds declares
// `private`.
//
// ok is false when the chain has no gnomod.toml at that path. That is a real
// answer rather than a failure: a package published before gnomod.toml
// replaced gno.mod has none, and there is nothing to disagree with. err stays
// reserved for a node that did not reply at all, per the seventh claim in
// CONTRIBUTING: collapsing the two would let an unreachable chain agree with
// everything.
func chainPrivate(c *Chain, module string) (private, ok bool, err error) {
	body, err := c.ABCIQuery("vm/qfile", module+"/gnomod.toml")
	if err != nil {
		if answered(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("asking %s for %s's gnomod.toml: %w", c.RPC, module, err)
	}
	return gnomodFlag([]byte(body), "private"), true, nil
}

// repoPrivate reports whether the working tree's copy of a package declares
// `private`. A package with no readable gnomod.toml is not a mismatch: the
// caller got this path out of the lock, so something else already has a better
// error for that.
func repoPrivate(root string, p Package) bool {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p.Dir), "gnomod.toml"))
	if err != nil {
		return false
	}
	return gnomodFlag(b, "private")
}

// CheckPrivate compares the `private` flag of every package in live against the
// copy the chain holds, and returns the ones that disagree, sorted by module so
// the report is stable.
//
// Only live packages are asked about. An absent one has no chain copy to
// disagree with, and a parked one has bytes that no longer answer vm/qfile as a
// live path would, so asking would produce a "chain has no gnomod" that means
// "not enabled yet" rather than "published without the flag".
//
// The error is a transport failure and stops the batch. A disagreement is a
// result, not an error, so that the caller decides what to do with it and the
// report can name all of them at once instead of the first.
func CheckPrivate(e *Env, c *Chain, root string, live []Package) ([]privateMismatch, error) {
	if len(live) == 0 {
		return nil, nil
	}
	var (
		mu   sync.Mutex
		out  []privateMismatch
		bad  error
		work = make(chan Package)
		wg   sync.WaitGroup
	)
	workers := probeConcurrency
	if len(live) < workers {
		workers = len(live)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				onChain, ok, err := chainPrivate(c, p.Module)
				mu.Lock()
				switch {
				case err != nil:
					if bad == nil {
						bad = err
					}
				case !ok:
					// No gnomod.toml on chain: nothing to compare.
				case onChain != repoPrivate(root, p):
					out = append(out, privateMismatch{
						module: p.Module, inRepo: !onChain, onChain: onChain,
					})
				}
				mu.Unlock()
			}
		}()
	}
	for _, p := range live {
		mu.Lock()
		stop := bad != nil
		mu.Unlock()
		if stop {
			break
		}
		work <- p
	}
	close(work)
	wg.Wait()
	if bad != nil {
		return nil, bad
	}
	sort.Slice(out, func(i, j int) bool { return out[i].module < out[j].module })
	e.tracef("private  compared %d live package(s) against %s, %d disagree\n", len(live), c.ID, len(out))
	return out, nil
}
