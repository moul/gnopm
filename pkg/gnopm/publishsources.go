package gnopm

import (
	"fmt"
	"path"
	"strings"
)

// publishSources is where `gnopm publish` may read a package's bytes from.
//
// Worked out once per run, from gnomod.lock, because the answer is needed three
// times: to decide what a pattern selects, to decide whether an import is this
// workspace's to satisfy, and to say why one is not.
//
// Two source variants qualify, for one reason: the bytes are this repository's
// own.
//
//   - { dir } is the version in the working tree, the one you edit.
//   - { commit, dir } is a version that LEFT the tree and is pinned to this
//     repository's own git history. `gnopm sync` materializes it under .gnopm/
//     and gnomod.lock carries its hash. Publishing it is the whole point of
//     pinning it: a live importer needs the version it imports to be live too,
//     and the superseded version has no directory of its own for anything to
//     find. publish used to filter to InTree(), which made it the one command
//     blind to the assembly that ls, why, doc and verify all read, and it
//     reported the result as "not live, and not in this workspace"
//     (moul/gno-meta#827).
//
// { chain } does NOT qualify, and that is a decision rather than an oversight.
// A chain-sourced module is somebody else's package: its bytes were read back
// off a chain with vm/qfile instead of written here, and its path sits in a
// namespace gno.land bound to whichever address first claimed it, so the
// add_package would be refused even if sending it were the right thing to do.
// The README draws that line for vendor/ already. A pinned version is this
// repository's past; a chain dependency is another author's present.
type publishSources struct {
	// pkg is every module this workspace may publish, by module path. For a
	// pinned version Dir is the assembly path, which is where the bytes are.
	pkg map[string]Package
	// pin is the lock entry of each pinned version, for the hash it carries
	// and for the commit and directory its [source] section must name.
	pin map[string]LockEntry
	// order is pkg's keys in lock order, and tree the { dir } subset of it.
	order []string
	tree  []string
	// why says, per module, why it is not publishable. Filled for a variant
	// that never qualifies, and lazily by ours() for a pinned version whose
	// materialized copy is absent or does not match the lock.
	why map[string]string
}

// publishSourcesOf splits the lock by source variant. It touches no files: a
// pinned version is proved by ours(), when something actually needs it.
func publishSourcesOf(lock *Lock) *publishSources {
	s := &publishSources{
		pkg: map[string]Package{},
		pin: map[string]LockEntry{},
		why: map[string]string{},
	}
	for _, m := range sortedModules(lock) {
		switch m.Source.Variant() {
		case "dir":
			s.pkg[m.Module] = Package{Dir: m.Source.Dir, Module: m.Module}
			s.order = append(s.order, m.Module)
			s.tree = append(s.tree, m.Module)
		case "commit":
			s.pkg[m.Module] = Package{Dir: assemblyRelOf(m.Module), Module: m.Module}
			s.pin[m.Module] = m
			s.order = append(s.order, m.Module)
		case "chain":
			s.why[m.Module] = fmt.Sprintf("published on %s by another author: not this repository's to publish", m.Source.Chain)
		default:
			s.why[m.Module] = fmt.Sprintf("source variant %q, which gnopm cannot read bytes for", m.Source.Variant())
		}
	}
	return s
}

// assemblyRelOf is assemblyPathOf as a repo-relative, slash-separated Dir,
// which is the form Package.Dir is documented to hold and the form every
// consumer in the publish path joins to e.Root.
func assemblyRelOf(module string) string {
	return path.Join(assemblyDir, module)
}

// ours answers "may this workspace publish this module", and for a pinned
// version PROVES it rather than trusting the directory: gnomod.lock carries a
// hash, so the bytes under .gnopm/ are checked against it before anything is
// sized, batched or signed. The assembly is generated, gitignored and
// rebuildable, which is exactly the kind of input a publish must not take on
// faith.
//
// A pinned version that fails leaves a reason in why, naming the command that
// fixes it, so its importer is blocked by something a reader can act on
// instead of by the flat "not in this workspace" that sent the last one
// looking for a version bump it did not need.
//
// The verdict is memoized: a pinned version is typically imported by several
// packages, and hashing it once per importer is work for no answer.
func (s *publishSources) ours(root, module string) (Package, bool) {
	p, known := s.pkg[module]
	if !known {
		return Package{}, false
	}
	en, pinned := s.pin[module]
	if !pinned {
		return p, true
	}
	if w, decided := s.why[module]; decided {
		return Package{}, w == ""
	}
	dir := assemblyPathOf(root, module)
	switch {
	case !dirExists(dir):
		s.why[module] = fmt.Sprintf("pinned to %s and not materialized: run `gnopm sync`", short(en.Source.Commit))
	default:
		h, err := hashDownloaded(dir)
		switch {
		case err != nil:
			s.why[module] = fmt.Sprintf("pinned to %s and its materialized copy cannot be read (%v): run `gnopm sync`", short(en.Source.Commit), err)
		case h != en.Hash:
			s.why[module] = fmt.Sprintf("pinned to %s but the copy under %s does not match gnomod.lock: run `gnopm sync`", short(en.Source.Commit), assemblyDir)
		default:
			s.why[module] = ""
		}
	}
	if s.why[module] != "" {
		return Package{}, false
	}
	return p, true
}

// selectBy is what a pattern names, in lock order.
//
// An empty pattern selects the working tree and not the pinned versions, and
// the asymmetry is the point. The tree is the set this workspace maintains;
// publishing every version it ever cut, unasked, would put code on chain that
// was deliberately removed from the tree and that nobody is looking at, at real
// gas and storage cost. A pinned version earns its place in the plan the same
// way a tree package pulled in as a dependency does: something being published
// imports it. Naming one explicitly, `gnopm publish p/moul/ulist/v0`, is how
// you publish one that nothing imports.
func (s *publishSources) selectBy(pattern string) []string {
	if pattern == "" {
		return append([]string(nil), s.tree...)
	}
	var out []string
	for _, mod := range s.order {
		p := s.pkg[mod]
		if strings.Contains(p.Module, pattern) || strings.Contains(p.Dir, pattern) {
			out = append(out, mod)
		}
	}
	return out
}
