package gnopm

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Vendoring: committing a chain dependency's source, so the repository carries
// every byte its build uses.
//
// The obvious design was to rewrite the lock entry to { dir = "vendor/..." },
// and it is wrong twice. scanPackages deliberately does not descend into
// vendor/, because vendored code is third-party and not yours to bump or
// publish, so a { dir } entry pointing there would fail `consistent` with
// "locked at vendor/x, where nothing declares it". And it would throw away the
// one thing worth keeping: which chain those bytes came from, and the hash that
// proves it.
//
// So vendoring does not touch the lock at all. The entry stays { chain }, and
// vendor/ becomes a third place the bytes can be found. The fill order is
//
//	vendor/  ->  the shared download cache  ->  the chain
//
// which means a vendored workspace needs no network, no cache and no chain, and
// `gnopm verify` still proves the same hash against whichever copy is in play.
// Unvendoring is deleting the directory.

// vendorDir is the committed third-party tree, at the workspace root.
//
// The same directory moul/gno-contracts and moul/gnopm-demo already keep by
// hand, and the one scanPackages skips.
const vendorDir = "vendor"

// vendorPathOf is where a module's vendored copy lives.
//
// Under the module path, not a flattened name, because that is what makes the
// directory readable: `vendor/gno.land/p/nt/tinyavl/v0` says what it is without
// opening anything, and gno resolves it from the module line regardless.
func vendorPathOf(root, module string) string {
	return filepath.Join(root, vendorDir, filepath.FromSlash(module))
}

// vendored reports the hash of a module's vendored copy, or "" if there is
// none. A directory that exists but does not hash right is not vendored: it is
// broken, and saying so is verify's job rather than this function's.
func vendored(root, module, want string) bool {
	h, err := hashDownloaded(vendorPathOf(root, module))
	return err == nil && h != "" && h == want
}

// Vendor copies every chain dependency into vendor/ and commits nothing.
//
// Writing the files is the whole operation: the lock already records what they
// are and what they must hash to, so there is nothing to rewrite and nothing
// that can drift. Removing the assembly copy is not tidiness either, it is
// required: two directories under the workspace root declaring one module path
// is precisely the ambiguity every other guard here exists to prevent.
func Vendor(e *Env) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	var chain []LockEntry
	for _, en := range lock.Modules {
		if en.Source.Variant() == "chain" {
			chain = append(chain, en)
		}
	}
	if len(chain) == 0 {
		e.logf("nothing to vendor: no dependency here comes from a chain.\n" +
			"  `gnopm get <package-path>` adds one; versions pinned to this\n" +
			"  repository's own history are already in its history.\n")
		return nil
	}

	done, already := 0, 0
	for _, en := range chain {
		dest := vendorPathOf(e.Root, en.Module)
		if vendored(e.Root, en.Module, en.Hash) {
			already++
			continue
		}
		// Reuse the assembly copy when it is the right one, so vendoring a
		// workspace that already synced costs no chain reads at all.
		src := filepath.Join(e.Root, assemblyDir, filepath.FromSlash(en.Module))
		if h, err := hashDownloaded(src); err != nil || h != en.Hash {
			c, err := DiscoverChain(e, en.Module, e.RPC, e.ChainID)
			if err != nil {
				return fmt.Errorf("%s: %w", en.Module, err)
			}
			src, _, err = download(e, c, en.Module, en.Hash)
			if err != nil {
				return err
			}
		}
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if err := copyDownload(src, dest); err != nil {
			return fmt.Errorf("%s: %w", en.Module, err)
		}
		e.logf("vendored %s\n  %s\n", en.Module, filepath.Join(vendorDir, filepath.FromSlash(en.Module)))
		done++
	}
	if done == 0 {
		e.logf("%d dependency(ies) already vendored, nothing to do\n", already)
		return nil
	}
	e.logf("%d vendored, %d already there.\n"+
		"  Commit %s/: it is source now, and this workspace needs no chain to build.\n"+
		"  Unvendoring is deleting the directory; `gnopm sync` falls back to the cache.\n",
		done, already, vendorDir)
	// Drop the assembly copies that vendor/ has replaced, so one module path
	// is declared by one directory.
	return Install(e)
}

func cmdVendor(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("vendor takes no arguments: it vendors every chain dependency in %s\n"+
			"  There is no partial vendoring on purpose: a tree that needs a chain for\n"+
			"  some of its dependencies still needs a chain", lockFile)
	}
	return Vendor(e)
}

// vendoredSignature is which chain dependencies are currently vendored, as a
// string, so the assembly stamp changes when that changes.
//
// It hashes the module paths rather than the contents: a vendored copy whose
// bytes are wrong is verify's business, and folding contents in here would make
// every install re-hash the whole vendor tree to answer a question about
// membership.
func vendoredSignature(root string, lock *Lock) string {
	var b strings.Builder
	for _, en := range lock.Modules {
		if en.Source.Variant() != "chain" {
			continue
		}
		if vendored(root, en.Module, en.Hash) {
			b.WriteString("\nvendored ")
			b.WriteString(en.Module)
		}
	}
	return b.String()
}
