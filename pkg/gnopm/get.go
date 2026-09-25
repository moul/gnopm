package gnopm

import (
	"flag"
	"fmt"
	"sort"
	"strings"
)

// Get adds a dependency that lives only on a chain.
//
// The command that writes; `sync` only satisfies what this has already decided.
// That split is principle 2 ("a build never mutates the lock") and it is the
// same one Go made when it defaulted to -mod=readonly: discovering a new
// dependency is an act, and an act belongs to a command you typed.
//
// What it records is deliberately small. A chain entry is the module path, the
// chain id and the content hash, and nothing about where the bytes sit on this
// machine, because that is the cache's business and two machines must be free
// to disagree about it while still agreeing the lock is satisfied.
func Get(e *Env, targets []string, rpc, chainID string) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	pkgs, err := scanPackages(e.Root)
	if err != nil {
		return err
	}
	inTree := map[string]string{}
	for _, p := range pkgs {
		inTree[p.Module] = p.Dir
	}

	byModule, err := lock.ByModule()
	if err != nil {
		return err
	}

	added, updated := 0, 0
	for _, target := range targets {
		path := strings.TrimSuffix(strings.TrimSpace(target), "/")
		if path == "" {
			continue
		}
		// Refusing to fetch something the workspace already builds is not
		// pedantry: two sources for one module path is the single way to make
		// the toolchain's resolution ambiguous, and it would be silent.
		if dir, ok := inTree[path]; ok {
			return fmt.Errorf("%s is already in this workspace, at %s.\n"+
				"  Nothing to fetch: a module path resolves to one place, and the tree wins", path, dir)
		}
		if en, ok := byModule[path]; ok && en.Source.Variant() == "commit" {
			return fmt.Errorf("%s is already pinned to this repository's history at %s.\n"+
				"  Fetching it from a chain would replace a version you can reproduce with one you cannot",
				path, short(en.Source.Commit))
		}

		c, err := DiscoverChain(e, path, rpc, chainID)
		if err != nil {
			return err
		}
		// No wantHash: this is the run that decides what the hash is. A
		// re-`get` of an already-locked path re-reads the chain rather than
		// trusting the cache, because re-running it is what somebody does when
		// they suspect the lock.
		_, hash, err := download(e, c, path, "")
		if err != nil {
			return err
		}

		if en, ok := byModule[path]; ok {
			if en.Source.Chain == c.ID && en.Hash == hash {
				e.logf("%s is already locked to %s, unchanged\n", path, c.ID)
				continue
			}
			en.Source = Source{Chain: c.ID}
			en.Hash = hash
			updated++
		} else {
			lock.Modules = append(lock.Modules, LockEntry{
				Module: path,
				Source: Source{Chain: c.ID},
				Hash:   hash,
			})
			added++
		}
		e.logf("%s from %s\n  %s\n", path, c.ID, hash)
	}
	if added == 0 && updated == 0 {
		return nil
	}
	lock.Sort()
	if err := writeLock(e.Root, lock); err != nil {
		return err
	}
	e.logf("%s updated: %d added, %d changed\n", lockFile, added, updated)
	// Sync, not Install. Install alone materializes the new dependency but
	// leaves any in-tree package the lock has not met yet unrecorded, so
	// `gnopm get` in a fresh workspace ended with `status` saying stale and
	// telling you to run the command `get` had just run half of.
	return Sync(e)
}

func cmdGet(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no package path given: `gnopm get gno.land/p/alice/md/v1`\n" +
			"  It is a full package path, because that is its address on a chain.\n" +
			"  `gnopm publish` names the ones this workspace imports and cannot resolve")
	}
	sort.Strings(args)
	return Get(e, args, flagString(fs, "rpc"), flagString(fs, "chainid"))
}
