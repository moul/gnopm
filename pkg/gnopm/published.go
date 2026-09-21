package gnopm

import (
	"fmt"
	"strings"
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
type Probe struct {
	chain     *Chain
	inert     map[string]bool
	haveInert bool
	cache     map[string]PackageState
}

// NewProbe resolves the chain a module path names and prepares to query it.
// rpc and chainID skip discovery, for a local gnodev.
func NewProbe(module, rpc, chainID string) (*Probe, error) {
	c, err := DiscoverChain(module, rpc, chainID)
	if err != nil {
		return nil, err
	}
	inert, have, err := inertSet(c)
	if err != nil {
		return nil, err
	}
	return &Probe{chain: c, inert: inert, haveInert: have, cache: map[string]PackageState{}}, nil
}

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
	if s, ok := p.cache[module]; ok {
		return s, nil
	}
	s, err := stateOf(p.chain, module, p.inert)
	if err != nil {
		return "", err
	}
	p.cache[module] = s
	return s, nil
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

func stateOf(c *Chain, path string, inert map[string]bool) (PackageState, error) {
	if inert[path] {
		return StateParked, nil
	}
	if _, err := c.ABCIQuery("vm/qfile", path); err != nil {
		if answered(err) {
			return StateAbsent, nil
		}
		return "", fmt.Errorf("asking %s about %s: %w", c.RPC, path, err)
	}
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
