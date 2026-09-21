package gnopm

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Has this version been published, and therefore does its number mean anything.
//
// A version that exists only in a git branch is an editing artefact. A version
// on a chain is a promise that cannot be taken back: an importer resolves
// against its path, a realm address derives from it, and `addpkg` on an
// occupied path fails, so its content can never be redefined either. That
// asymmetry is the whole reason bump, unbump and tidy all ask the same
// question, and it has exactly one answer, from the chain the package path
// names.
//
// Nothing here writes. Reading a chain is free and gnopm does it freely;
// changing one is a gnokey command printed for a human.

// Probe answers that question for a set of module paths, over one chain,
// reading the parked set once and caching every path it looks up.
//
// Two caches, and the difference between them is the point. cache is this
// run's answers, all three states, discarded when the process exits. cached is
// the disk set of paths this chain has served before, which survives runs
// because "live" is the one answer that cannot later turn out to be wrong.
type Probe struct {
	env       *Env
	chain     *Chain
	inert     map[string]bool
	haveInert bool
	// cached is the disk set of paths this chain has already served, which
	// outlives the process because "live" is the one answer that cannot later
	// turn out to be wrong. Its own lock, so Warm's workers can feed it.
	cached *liveCache

	// mu guards cache alone. inert, haveInert and cached are written once by
	// NewProbe and safe for concurrent reads afterwards, which is what lets
	// Warm share them across workers without a lock on the hot path.
	mu    sync.Mutex
	cache map[string]PackageState
}

// probeConcurrency is how many chain reads are in flight at once.
//
// The ceiling is not the local machine, it is the public endpoint: a chain
// read is cheap for a node and a stranger's node is not ours to hammer. Eight
// turns a minute of serial round trips into a few seconds and stays well under
// any rate limit worth having. MaxIdleConnsPerHost is set to the same number,
// so every worker gets a connection and none of them handshakes twice.
const probeConcurrency = 8

// NewProbe resolves the chain a module path names and prepares to query it.
// rpc and chainID skip discovery, for a local gnodev.
func NewProbe(e *Env, module, rpc, chainID string) (*Probe, error) {
	c, err := DiscoverChain(e, module, rpc, chainID)
	if err != nil {
		return nil, err
	}
	cached := openLiveCache(e.cacheDir(), c.ID)
	if f := cached.where(); f != "" {
		start, _, _ := cached.stats()
		e.tracef("cache    %s, %d path(s) known live\n", f, start)
	} else {
		e.tracef("cache    off: nothing about %s is kept between runs\n", c.ID)
	}
	start := time.Now()
	inert, have, err := inertSet(c)
	if err != nil {
		return nil, err
	}
	e.tracef("chain    vm/qinertpaths: %d path(s) parked, in %s\n", len(inert), took(start))
	return &Probe{
		env: e, chain: c, inert: inert, haveInert: have,
		cache: map[string]PackageState{}, cached: cached,
	}, nil
}

// Cache is the disk set backing this probe, so a command can report what it
// saved. Never nil.
func (p *Probe) Cache() *liveCache { return p.cached }

// Chain is what the probe resolved, so a caller can name it in its output.
// A message that says "absent" without saying absent from where is a message
// that invites the reader to check the wrong chain.
func (p *Probe) Chain() *Chain { return p.chain }

// CanPark reports whether this chain has the inert submission policy. Where it
// does not, absent really is absent; where it does, a green broadcast is not
// the same thing as live.
func (p *Probe) CanPark() bool { return p.haveInert }

// State reports what the chain says about one module path. The error is a
// transport failure and never a chain answer: a chain that says "no such
// package" returns StateAbsent with a nil error, and a chain that could not be
// reached returns an error rather than pretending the package is absent.
func (p *Probe) State(module string) (PackageState, error) {
	p.mu.Lock()
	s, ok := p.cache[module]
	p.mu.Unlock()
	if ok {
		return s, nil
	}
	s, err := p.resolve(module)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.cache[module] = s
	p.mu.Unlock()
	return s, nil
}

// resolve answers one path from wherever the answer is cheapest, and says
// where under -v. Safe to call from several goroutines; it does not touch
// p.cache, which is the caller's business.
func (p *Probe) resolve(module string) (PackageState, error) {
	if s, where, ok := p.answerLocally(module); ok {
		p.trace(s, module, where)
		return s, nil
	}
	start := time.Now()
	s, err := stateOf(p.chain, module, p.inert, p.cached)
	if err != nil {
		return "", err
	}
	p.trace(s, module, "chain, "+took(start))
	return s, nil
}

// answerLocally resolves a path without touching the network: from the parked
// set read once at startup, or from the disk set of paths this chain has
// already served.
//
// Splitting this out of stateOf is what lets Warm size its batch by the work
// that genuinely needs a round trip, so the progress bar counts real queries
// and the workers are not dispatched to do a map lookup.
func (p *Probe) answerLocally(module string) (PackageState, string, bool) {
	if p.inert[module] {
		return StateParked, "parked set", true
	}
	if p.cached.has(module) {
		return StateLive, "cache", true
	}
	return "", "", false
}

// trace is the -v line for one answer. It takes p.mu because Warm calls it
// from several goroutines and two half-written lines are worse than none.
func (p *Probe) trace(s PackageState, module, where string) {
	if p.env == nil || !p.env.Verbose {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.env.tracef("  %-6s %s  (%s)\n", s, module, where)
}

// Warm resolves many module paths at once and fills the cache, so that the
// State calls which follow are local.
//
// This exists because the shape of the work is a batch: publish knows every
// path it cares about before it asks about any of them, and asking serially
// spends one network round trip per package for no reason. Nothing about the
// answers interacts, so the batch parallelizes exactly.
//
// step, when non-nil, is called once per completed query with a running count
// and the path just resolved. It is called from several goroutines, so an
// implementation has to be safe for that; progress.step is.
//
// The error is the first transport failure, and the batch stops asking once it
// has one. A chain answer is never an error here, same as State: the point of
// the distinction is that an unreachable node must not read as an empty one.
func (p *Probe) Warm(modules []string, step func(done, total int, module string)) error {
	seen := map[string]bool{}
	var todo []string
	p.mu.Lock()
	for _, m := range modules {
		if _, cached := p.cache[m]; cached || seen[m] {
			continue
		}
		seen[m] = true
		todo = append(todo, m)
	}
	p.mu.Unlock()
	if len(todo) == 0 {
		return nil
	}
	// Sorted so the path shown next to the bar advances in a readable order
	// rather than in whichever order the lock happened to hold.
	sort.Strings(todo)

	// Everything the parked set or the disk cache can answer is answered here,
	// before a single worker starts. On a warm cache that is nearly all of it,
	// and what remains is the batch worth parallelizing.
	cold := todo[:0:0]
	for _, m := range todo {
		s, where, ok := p.answerLocally(m)
		if !ok {
			cold = append(cold, m)
			continue
		}
		p.trace(s, m, where)
		p.mu.Lock()
		p.cache[m] = s
		p.mu.Unlock()
	}
	if len(cold) == 0 {
		return nil
	}
	todo = cold

	workers := probeConcurrency
	if len(todo) < workers {
		workers = len(todo)
	}
	work := make(chan string)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
		done     int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range work {
				errMu.Lock()
				stop := firstErr != nil
				errMu.Unlock()
				if stop {
					continue // drain, so the producer is never left blocked
				}
				start := time.Now()
				s, err := stateOf(p.chain, m, p.inert, p.cached)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					continue
				}
				p.mu.Lock()
				p.cache[m] = s
				p.mu.Unlock()
				p.trace(s, m, "chain, "+took(start))
				if step != nil {
					errMu.Lock()
					done++
					n := done
					errMu.Unlock()
					step(n, len(todo), m)
				}
			}
		}()
	}
	for _, m := range todo {
		work <- m
	}
	close(work)
	wg.Wait()
	return firstErr
}

// Published reports whether the chain has this version in any form a later
// change has to respect. Parked counts: the bytes were accepted and are
// waiting for an approver, so they cannot be edited and must not be sent
// again. Treating parked as unpublished is how a submission gets duplicated.
func (p *Probe) Published(module string) (bool, error) {
	s, err := p.State(module)
	if err != nil {
		return false, err
	}
	return s != StateAbsent, nil
}

// inertSet reads every parked path once.
//
// A chain without the inert policy has no such endpoint, and answers so. That
// is not an error: it means nothing can be parked, so the set is empty and
// every absent package is genuinely absent. A chain that could not be reached
// is a different thing entirely and is returned as one, because silently
// reading it as "nothing is parked" would make a parked package look editable.
func inertSet(c *Chain) (map[string]bool, bool, error) {
	raw, err := c.ABCIQuery("vm/qinertpaths", "")
	if err != nil {
		if answered(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	set := map[string]bool{}
	for _, l := range strings.Split(raw, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			set[l] = true
		}
	}
	return set, true, nil
}

// stateOf asks the chain, and feeds the disk cache from here rather than from
// its callers so that no path into it can forget to write back what it just
// learned. cached may be nil, which is a probe that asks the chain every time.
//
// The inert check stays as a guard for a caller that has not been through
// answerLocally; asking vm/qfile about a parked path would answer "absent".
func stateOf(c *Chain, path string, inert map[string]bool, cached *liveCache) (PackageState, error) {
	if inert[path] {
		return StateParked, nil
	}
	if _, err := c.ABCIQuery("vm/qfile", path); err != nil {
		if answered(err) {
			return StateAbsent, nil
		}
		return "", fmt.Errorf("asking %s about %s: %w", c.RPC, path, err)
	}
	cached.learn(path)
	return StateLive, nil
}

// maxVersionWalk bounds the descent in FirstFreeVersion. Versions are typed by
// hand and `bump -to 1000` is a typo, not a plan; without a bound that typo is
// a thousand chain reads.
const maxVersionWalk = 64

// FirstFreeVersion returns the lowest version of base that nothing has taken
// and nothing ever can: one above the highest published version below cur.
//
// This, and not cur-1, is what a version in the working tree should be
// numbered. A version number is a tag on something published, so the numbers
// between the last published one and the tree's are tags on nothing: they name
// no bytes, resolve for no importer, and derive no address. Whatever the tree
// holds is going to be published as the next number after the last real one,
// and every number above that is a hole somebody will later try to explain.
//
// It walks down and stops at the first published version rather than counting
// up, so a sequence with a hole in it, v0 and v2 published and v1 never, still
// answers with the highest, which is the only one that constrains what comes
// next.
func FirstFreeVersion(p *Probe, base string, cur int) (int, error) {
	stop := cur - maxVersionWalk
	for n := cur - 1; n >= 0; n-- {
		if n < stop {
			return 0, fmt.Errorf("gave up looking for a published version of %s below v%d after %d tries",
				base, cur, maxVersionWalk)
		}
		published, err := p.Published(fmt.Sprintf("%s/v%d", base, n))
		if err != nil {
			return 0, err
		}
		if published {
			return n + 1, nil
		}
	}
	return 0, nil
}
