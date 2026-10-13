package gnopm

import (
	"fmt"
	"sort"
	"strings"
)

// Editing a published version in place is the one mistake the lock cannot see.
//
// `bump -if-published` exists so an author never has to know by heart whether a
// version is on a chain. Nothing made them run it, and in September 2026 a
// repository shipped a rendering fix straight into a realm whose version was
// live: a public package path cannot be redeployed, so the tree's v0 and the
// chain's v0 diverged permanently and no check said a word. The lock was
// consistent throughout, every pin reproduced, and nothing survived a squash
// merge wrongly. This file is the check that was missing.
//
// It warned rather than failed, and that was wrong. moul/gno-contracts#331
// carried this exact warning, naming all five packages and saying in so many
// words that the edit could never reach a chain; it was merged anyway, and five
// days later three of those paths were still serving the uncapped code the pull
// request was written to fix (gno-contracts#331, 2026-10-08 to 2026-10-13). A
// warning on a thing that cannot work is a warning nobody has to act on.
//
// So the verdict now depends on what the chain would actually accept, which is
// the one question with a real answer:
//
//   - absent: the version was published to nobody, so v0 is still the version
//     to edit. Silent, and the common case.
//   - live and `private`: a second MsgAddPackage at the same path is allowed,
//     so the edit CAN reach the chain. Reported, because the redeploy re-runs
//     init() and wipes realm state, and never failed, because editing in place
//     is the only route such a package has.
//   - anything else: no transaction will ever carry this edit. That is a
//     failure, and there is no good reason to hold the opposite: the tree would
//     be describing code that is not running and never will.

// editedInPlace returns the packages whose PRODUCTION source this branch
// changes without moving their module line.
//
// Production source only: a _test.gno or _filetest.gno never reaches a chain,
// so changing one cannot make the tree disagree with what is deployed. Neither
// can a README or the gnomod.toml itself, and a package the branch adds has no
// published version to contradict.
func editedInPlace(root string, pkgs []Package, base string) ([]Package, error) {
	// Three dots: what this branch changed, not what the base moved on to
	// underneath it. Two dots would name every package someone else touched.
	out, err := git(root, "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	var touched []string
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); isProdGno(f) {
			touched = append(touched, f)
		}
	}
	if len(touched) == 0 {
		return nil, nil
	}

	var edited []Package
	for _, p := range pkgs {
		if p.Ignored || !anyFileUnder(touched, p.Dir) {
			continue
		}
		was, err := moduleAt(root, base, p.Dir)
		if err != nil {
			// No gnomod.toml at base means the branch adds this package, which
			// is the one case where there is nothing to have bumped.
			continue
		}
		if was != p.Module {
			continue // already bumped, which is the whole point
		}
		edited = append(edited, p)
	}
	sort.Slice(edited, func(i, j int) bool { return edited[i].Module < edited[j].Module })
	return edited, nil
}

// isProdGno reports whether a path is gno source the VM actually runs in
// production, as opposed to a test.
//
// Not "what gets deployed": a _test.gno travels with the package and is charged
// for, which is why Payload counts it. What this excludes is the files whose
// declarations never run and whose package clause may be something else
// entirely, so it is the right filter for "is this a behaviour change" and for
// "which file names the package".
func isProdGno(f string) bool {
	return strings.HasSuffix(f, ".gno") &&
		!strings.HasSuffix(f, "_test.gno") &&
		!strings.HasSuffix(f, "_filetest.gno")
}

// anyFileUnder reports whether any path sits directly inside dir. Directly,
// not recursively: p/moul/kit and p/moul/kit/ui are two packages, and a change
// to the second is not a change to the first.
func anyFileUnder(files []string, dir string) bool {
	for _, f := range files {
		if strings.HasPrefix(f, dir+"/") && !strings.Contains(f[len(dir)+1:], "/") {
			return true
		}
	}
	return false
}

// moduleAt reads a package's declared module path as of a git ref.
func moduleAt(root, ref, dir string) (string, error) {
	b, err := git(root, "show", ref+":"+dir+"/gnomod.toml")
	if err != nil {
		return "", err
	}
	module, _, err := parseGnomod([]byte(b), ref+":"+dir+"/gnomod.toml")
	return module, err
}

// editVerdict is one package edited in place, and what the chain says about the
// version it still declares.
type editVerdict struct {
	Pkg   Package
	State PackageState
	// Redeployable is true only for a live path whose chain copy declares
	// `private`. The flag binds at the first publish and cannot be changed
	// after, so it is read from the chain and never from the working tree: a
	// tree that added it afterwards is describing a promise it never made, and
	// the transaction would be refused as "package already exists".
	Redeployable bool
}

// classifyEdits asks the chain about every package edited in place.
//
// state and redeployable are injected rather than taken as a *Probe so the
// report path is testable without a network: CI passes the probe's methods,
// tests pass answers. Same reason the previous shape took a `published` func,
// and the same seventh claim in CONTRIBUTING applies to both: an error here is
// a node that did not reply, never a node that said no.
func classifyEdits(
	edited []Package,
	state func(module string) (PackageState, error),
	redeployable func(module string) (bool, error),
) ([]editVerdict, error) {
	var out []editVerdict
	for _, p := range edited {
		st, err := state(p.Module)
		if err != nil {
			return nil, err
		}
		if st == StateAbsent {
			continue // published to nobody, so this is the version to edit
		}
		v := editVerdict{Pkg: p, State: st}
		// Only a live path has a gnomod.toml to read. A parked one is a
		// submission an approver can still reject, and its bytes do not answer
		// vm/qfile the way a live path's do, so asking would produce "the chain
		// has no gnomod.toml" and mean something else entirely.
		if st == StateLive {
			priv, err := redeployable(p.Module)
			if err != nil {
				return nil, err
			}
			v.Redeployable = priv
		}
		out = append(out, v)
	}
	return out, nil
}

// split separates the verdicts that can never reach a chain from the ones that
// can, because they are two different sentences with two different fixes.
func split(vs []editVerdict) (frozen, redeployable []editVerdict) {
	for _, v := range vs {
		if v.Redeployable {
			redeployable = append(redeployable, v)
			continue
		}
		frozen = append(frozen, v)
	}
	return frozen, redeployable
}

// frozenEditError is the sentence that fails the run. It names the command,
// because an author who has to look one up will instead do nothing.
func frozenEditError(vs []editVerdict) string {
	is, why := "is", "published and not `private`, so no transaction can ever carry this edit"
	if len(vs) > 1 {
		is = "are"
	}
	if allParked(vs) {
		why = "parked: the bytes an approver is looking at are not these, and they cannot be re-sent"
	}
	return fmt.Sprintf("%s %s %s. `gnopm bump -if-published %s`", modules(vs), is, why, vs[0].Pkg.Dir)
}

// allParked reports whether every verdict is a parked submission, which is the
// one case with a different reason and the same fix.
func allParked(vs []editVerdict) bool {
	for _, v := range vs {
		if v.State != StateParked {
			return false
		}
	}
	return len(vs) > 0
}

// redeployNote is the sentence that does not fail the run: the edit is the only
// route this package has, and the cost is the one thing the author has to weigh.
func redeployNote(vs []editVerdict) string {
	declares := "declares"
	if len(vs) > 1 {
		declares = "declare"
	}
	return fmt.Sprintf("%s %s `private` on the chain, so the edit reaches it only through a redeploy, "+
		"which re-runs init() and resets realm state. `gnopm publish`", modules(vs), declares)
}

// modules renders a verdict list as the backticked module paths, in the order
// editedInPlace sorted them.
func modules(vs []editVerdict) string {
	names := make([]string, 0, len(vs))
	for _, v := range vs {
		names = append(names, "`"+v.Pkg.Module+"`")
	}
	return strings.Join(names, ", ")
}
