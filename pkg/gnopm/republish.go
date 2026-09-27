package gnopm

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Replacing a package that is already on chain.
//
// publish answers "what is absent", which is the right question exactly once
// per package. A realm at a path an external consumer hard-codes never gets a
// second version: gno.land/r/moul/home is what gnoweb calls for /u/moul, so it
// carries no /vN and the only way to ship a code change is to send
// MsgAddPackage at the same path again.
//
// The chain allows that for a package whose mempackage declared `private`, and
// binds it to the address in [addpkg].creator. Everything else about the
// transaction is identical to a first publish, which is why this file adds a
// reason to include a package in the plan and nothing else: the payload, the
// gas, the batching and the handoff are unchanged.
//
// It is opt-in because it is destructive in a way a first publish is not. A
// redeploy re-runs init() and RESETS realm state, so every value the realm
// accumulated since it went up is gone. Nothing gnopm can read tells it
// whether that state mattered, so nothing gnopm does should decide it.

// republishCheck is the answer for one already-live package: whether it can be
// replaced, whether it needs to be, and what to tell the reader either way.
type republishCheck struct {
	// eligible is true only when the chain would accept the redeploy AND the
	// source actually differs. Both halves are required: the first because a
	// public package's redeploy is refused, the second because re-sending
	// identical bytes costs gas and wipes state to arrive where it started.
	eligible bool
	// why explains a no. Empty when eligible.
	why string
	// changed names the files that differ, for the report. A reader deciding
	// whether to wipe realm state deserves to see what they are buying.
	changed []string
}

// checkRepublish compares the working tree's copy of p against the chain's.
//
// Two queries per package: the gnomod.toml for the private flag, then the file
// list, then one per differing name. That is more round trips than the rest of
// publish spends on a package, and it is why this runs only for packages the
// pattern selected and only under -republish.
func checkRepublish(c *Chain, root string, p Package) (republishCheck, error) {
	private, ok, err := chainPrivate(c, p.Module)
	if err != nil {
		return republishCheck{}, err
	}
	switch {
	case !ok:
		// Published before gnomod.toml existed, so the flag was never carried
		// and the chain holds no claim either way. Refusing is the safe read:
		// the transaction would be rejected and the only way to find out for
		// certain is to send it.
		return republishCheck{why: "the chain's copy has no gnomod.toml, so it declared no private flag: " +
			"a redeploy would be refused as \"package already exists\""}, nil
	case !private:
		return republishCheck{why: "the chain's copy is public: AddPackage refuses a second package at the same path, " +
			"and the flag binds at the first publish and cannot be changed after"}, nil
	}

	onChain, err := chainSource(c, p.Module)
	if err != nil {
		return republishCheck{}, err
	}
	local, err := localSource(root + "/" + p.Dir)
	if err != nil {
		return republishCheck{}, err
	}

	changed := diffSource(local, onChain)
	if len(changed) == 0 {
		return republishCheck{why: "identical to the chain's copy, byte for byte: " +
			"a redeploy would spend gas and reset realm state to arrive where it already is"}, nil
	}
	return republishCheck{eligible: true, changed: changed}, nil
}

// chainSource reads every file of the deployed package, keyed by name.
//
// vm/qfile on a package path answers a newline-separated list of file names,
// and on a path with a file appended answers that file's bytes. A package that
// is not there at all is an ABCI error, which the caller has already ruled out
// by the time this runs.
func chainSource(c *Chain, module string) (map[string]string, error) {
	listing, err := c.ABCIQuery("vm/qfile", module)
	if err != nil {
		return nil, fmt.Errorf("listing %s on %s: %w", module, c.RPC, err)
	}
	out := map[string]string{}
	for _, name := range strings.Split(strings.TrimSpace(listing), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		body, err := c.ABCIQuery("vm/qfile", module+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("reading %s/%s on %s: %w", module, name, c.RPC, err)
		}
		out[name] = body
	}
	return out, nil
}

// localSource reads the payload as it would be uploaded, keyed by the flat
// name the message carries. It deliberately reuses payloadFiles rather than
// walking the directory again: the set compared has to be the set sent, or a
// file that travels without being compared makes "identical" a lie.
func localSource(dir string) (map[string]string, error) {
	files, err := payloadFiles(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return nil, err
		}
		out[f.Name] = string(b)
	}
	return out, nil
}

// diffSource names every file that differs, in a stable order, marking which
// side it is missing from. A rename shows up as one added and one removed,
// which is what happened as far as the chain is concerned.
func diffSource(local, onChain map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for name, body := range local {
		seen[name] = true
		switch other, ok := onChain[name]; {
		case !ok:
			out = append(out, name+" (new)")
		case other != body:
			out = append(out, name)
		}
	}
	for name := range onChain {
		if !seen[name] {
			out = append(out, name+" (removed)")
		}
	}
	sort.Strings(out)
	return out
}
