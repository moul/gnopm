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
// the chain to decide what is absent.
//
// The chain half is asked once per path and then remembered. The first cut of
// this check re-asked for every live package on every run, which on a
// 243-package workspace is 243 extra queries per publish that the disk cache
// did not absorb, and two whole-workspace runs in quick succession was enough
// for rpc.gno.land to start answering this host 403 on everything. The irony
// was exact: probeConcurrency carries a comment saying a stranger's node is not
// ours to hammer, and this honoured the concurrency while defeating the point.
// The answer cannot change (see cache.go), so it belongs on disk.

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
	cached := openPrivateCache(e.cacheDir(), c.ID)

	// Split before spawning anything: a path the cache already knows needs no
	// worker, no round trip and no concurrency slot. On a warm workspace this
	// empties the queue entirely and the chain is not touched at all.
	var ask []Package
	var out []privateMismatch
	for _, p := range live {
		st, ok := cached.lookup(p.Module)
		if !ok {
			ask = append(ask, p)
			continue
		}
		if m, mismatch := comparePrivate(root, p, st); mismatch {
			out = append(out, m)
		}
	}
	if len(ask) == 0 {
		sort.Slice(out, func(i, j int) bool { return out[i].module < out[j].module })
		e.tracef("private  %d live package(s), all remembered from %s, %d disagree\n",
			len(live), cacheOrMemory(cached), len(out))
		return out, nil
	}

	var (
		mu   sync.Mutex
		bad  error
		work = make(chan Package)
		wg   sync.WaitGroup
	)
	workers := probeConcurrency
	if len(ask) < workers {
		workers = len(ask)
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
				default:
					st := privateAbsent
					if ok {
						st = privatePublic
						if onChain {
							st = privatePrivate
						}
					}
					// Learn before comparing: the answer is the chain's and is
					// worth keeping whether or not it disagrees with the repo,
					// and the repo half can change between runs while this
					// cannot.
					cached.learn(p.Module, st)
					if m, mismatch := comparePrivate(root, p, st); mismatch {
						out = append(out, m)
					}
				}
				mu.Unlock()
			}
		}()
	}
	for _, p := range ask {
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
	_, hits, learned := cached.stats()
	e.tracef("private  compared %d live package(s) against %s, %d disagree (%d asked, %d remembered, %d learned)\n",
		len(live), c.ID, len(out), len(ask), hits, learned)
	return out, nil
}

// comparePrivate turns one chain answer into a mismatch, or not.
//
// Split out because the cached path and the freshly-queried path have to reach
// the same verdict, and two copies of a comparison eventually disagree.
func comparePrivate(root string, p Package, st privateState) (privateMismatch, bool) {
	if st == privateAbsent {
		// No gnomod.toml on chain: nothing to compare.
		return privateMismatch{}, false
	}
	onChain := st == privatePrivate
	if onChain == repoPrivate(root, p) {
		return privateMismatch{}, false
	}
	return privateMismatch{module: p.Module, inRepo: !onChain, onChain: onChain}, true
}

// cacheOrMemory names where the answers came from, for -v. A cache with no file
// still answers within a run, and saying "the cache" when nothing persists
// would make a warm-looking run impossible to tell from a cold one.
func cacheOrMemory(c *privateCache) string {
	if f := c.where(); f != "" {
		return f
	}
	return "memory"
}
