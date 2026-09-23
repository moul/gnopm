package gnopm

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Grouping a publish into dependency layers, so N packages cost fewer than N
// signatures.
//
// A layer is an antichain: every package in it is independent of every other
// package in the same layer, so the order they execute in cannot matter and
// they can share one transaction. Layer 0 is everything whose workspace
// imports are already satisfied, layer 1 is what depends only on layer 0, and
// so on.
//
// Batching the whole dependency graph into one transaction would be fewer
// prompts still, and messages in a transaction do share a store and run in
// order (`BaseApp.runMsgs`), so in principle a dependent could follow its
// dependency inside one transaction. It is not worth relying on: a chain
// running an inert submission policy PARKS a submission instead of making it
// live, so the dependency the next message needs is not there to import.
// Layering is correct under both, and costs a handful of extra signatures.

// layerPlans groups the publishable plans into dependency layers. deps maps a
// module to the imports read for it; anything not in the publish set is
// ignored, because it is either already live or somebody else's to deploy,
// and either way it does not order this run.
//
// The input order is preserved inside each layer, so a run is reproducible.
// A cycle cannot survive here: TopoOrder has already rejected one by the time
// plans exist, and a cycle that somehow arrived would leave packages unplaced,
// which is returned as an error rather than silently dropped.
func layerPlans(plans []plan, deps map[string][]string) ([][]plan, error) {
	inSet := map[string]bool{}
	var todo []plan
	for _, pl := range plans {
		if pl.state != StateAbsent || len(pl.missing) > 0 {
			continue
		}
		inSet[pl.pkg.Module] = true
		todo = append(todo, pl)
	}
	if len(todo) == 0 {
		return nil, nil
	}

	placed := map[string]bool{}
	var layers [][]plan
	remaining := todo
	for len(remaining) > 0 {
		var layer, next []plan
		for _, pl := range remaining {
			ready := true
			for _, d := range deps[pl.pkg.Module] {
				if d != pl.pkg.Module && inSet[d] && !placed[d] {
					ready = false
					break
				}
			}
			if ready {
				layer = append(layer, pl)
			} else {
				next = append(next, pl)
			}
		}
		if len(layer) == 0 {
			names := make([]string, 0, len(next))
			for _, pl := range next {
				names = append(names, pl.pkg.Module)
			}
			return nil, fmt.Errorf("dependency cycle among %d package(s): %s",
				len(next), strings.Join(names, ", "))
		}
		// Marking after the whole layer is chosen, not during, is what makes
		// a layer an antichain: marking as we go would let a package join the
		// same layer as the dependency it was just told about.
		for _, pl := range layer {
			placed[pl.pkg.Module] = true
		}
		layers = append(layers, layer)
		remaining = next
	}
	return layers, nil
}

// addrLine matches the address in one `gnokey list` row, whose format is
// `%d. %s (%s) - addr: %v pub: %v, path: %v` (tm2 list.go).
var addrLine = regexp.MustCompile(`^\s*\d+\.\s+(\S+)\s+\([^)]*\)\s+-\s+addr:\s+(g1\w+)`)

// creatorFor resolves the address that will sign, which every MsgAddPackage
// carries as a field and gnopm therefore has to know before it can write one.
//
// It asks gnokey rather than reading a keybase file: gnopm holds no key and
// parses no wallet, and asking the tool that owns the keybase keeps it that
// way. An explicit -addr wins, and a key that is already an address is its own
// answer.
func creatorFor(gnokeyCmd, key, addr string) (string, error) {
	if addr != "" {
		if !bech32ish(addr) {
			return "", fmt.Errorf("%q does not look like an address: -addr takes a g1... address, not a key name", addr)
		}
		return addr, nil
	}
	if bech32ish(key) {
		return key, nil
	}
	out, err := exec.Command(gnokeyCmd, "list").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s list: %w", gnokeyCmd, err)
	}
	if a := addrOf(string(out), key); a != "" {
		return a, nil
	}
	return "", fmt.Errorf("no key named %q in `%s list`", key, gnokeyCmd)
}

// addrOf finds the address of the named key in `gnokey list` output.
func addrOf(listing, key string) string {
	for _, line := range strings.Split(listing, "\n") {
		m := addrLine.FindStringSubmatch(line)
		if m != nil && m[1] == key {
			return m[2]
		}
	}
	return ""
}

// defaultTxPath is where the layered transaction documents go when -o did not
// say. They are build output, not something to keep: each one is only valid
// for the account sequence it was signed against, so a stale copy is worse
// than no copy.
func defaultTxPath(e *Env) string {
	dir := e.cacheDir()
	if dir == "" {
		dir = filepath.Join(e.Root, ".gnopm")
	}
	return filepath.Join(dir, "publish", "tx.json")
}
