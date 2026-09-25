package gnopm

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Downloading a dependency that lives only on a chain, into a directory shared
// by every workspace on this machine.
//
// The shape is `go mod download`'s, and the reason it can be simpler is worth
// stating once. A package path on a chain is immutable by construction: there
// is no delete, and addpkg on an occupied path fails, so the bytes at a path
// can never be redefined. The same fact ADR 0031 built the live cache on.
//
// Three things follow, and together they are why there is no resolver here and
// never will be:
//
//   - a content hash pins something that genuinely cannot change, so a cache
//     entry cannot be invalidated by an upstream edit, only by corruption;
//   - there is no version range to solve, because a gno import path carries its
//     version in the path;
//   - there is no registry to trust or to go offline, because the chain is the
//     registry.
//
// The download directory is therefore a pure optimisation over "ask the chain
// again", never a source of truth, and principle 8 holds: a workspace whose
// dependencies are all in the tree or vendored must never touch either.

// downloadSubdir is where fetched packages live under the cache root, beside
// hosts/, live/ and publish/.
const downloadSubdir = "download"

// metaFile records what was fetched and from where.
//
// It is written for a human and for `gnopm env`, not parsed back as the source
// of truth: the hash in gnomod.lock decides whether a download is right, and a
// metadata file that disagreed with it would just be a second opinion nobody
// asked for. Deliberately not a name gno will ever read as source.
const metaFile = ".gnopm-meta"

// downloadPath is where one package's files land.
//
// Keyed by chain id first, because the same path on two chains is two different
// packages and conflating them is the one mistake this layout must make
// impossible.
func downloadPath(cacheRoot, chainID, pkgPath string) string {
	return filepath.Join(cacheRoot, downloadSubdir, chainID, filepath.FromSlash(pkgPath))
}

// fetchedPackage is what one vm/qfile round produces.
type fetchedPackage struct {
	Files map[string]string // name -> body
	Hash  string
}

// names returns the file names, sorted, which is the order the hash is over.
func (f fetchedPackage) names() []string {
	out := make([]string, 0, len(f.Files))
	for n := range f.Files {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// fetchPackage reads a package's source back off a chain.
//
// Two query shapes, both vm/qfile, verified against gno master 8773794,
// gno.land/pkg/sdk/vm/keeper.go QueryFile:
//
//	vm/qfile <pkgpath>          the file names, newline separated
//	vm/qfile <pkgpath>/<name>   that file's body
//
// The listing comes from GetMemPackageAll, so it includes _test.gno and
// _filetest.gno. They are kept: they are part of what was deployed, `gno test`
// on a dependency is a reasonable thing to want, and dropping them here would
// make the hash of a downloaded package differ from the hash of the same
// package vendored, for no reason a user could see.
func fetchPackage(c *Chain, pkgPath string) (fetchedPackage, error) {
	listing, err := c.ABCIQuery("vm/qfile", pkgPath)
	if err != nil {
		return fetchedPackage{}, fmt.Errorf("listing %s on %s: %w", pkgPath, c.ID, err)
	}
	var names []string
	for _, n := range strings.Split(listing, "\n") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return fetchedPackage{}, fmt.Errorf("%s on %s lists no files", pkgPath, c.ID)
	}
	out := fetchedPackage{Files: make(map[string]string, len(names))}
	for _, n := range names {
		// A name with a separator in it would escape the package directory
		// when written. The chain has no business sending one, which is
		// exactly why it is checked here rather than assumed.
		if n != path.Base(n) || n == "." || n == ".." {
			return fetchedPackage{}, fmt.Errorf("%s on %s lists a file named %q, which is not a plain file name", pkgPath, c.ID, n)
		}
		body, err := c.ABCIQuery("vm/qfile", pkgPath+"/"+n)
		if err != nil {
			return fetchedPackage{}, fmt.Errorf("reading %s/%s on %s: %w", pkgPath, n, c.ID, err)
		}
		out.Files[n] = body
	}
	h, err := hashFiles(out.names(), func(name string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(out.Files[name])), nil
	})
	if err != nil {
		return fetchedPackage{}, err
	}
	out.Hash = h
	return out, nil
}

// download makes a package present in the shared cache and returns where.
//
// wantHash is the lock's recorded hash, or "" when there is nothing to check
// against yet, which is only true of `gnopm get` the first time. A cached
// directory whose hash does not match is treated as corruption and refetched
// rather than trusted or deleted-and-failed: the chain is authoritative and
// costs one round trip.
func download(e *Env, c *Chain, pkgPath, wantHash string) (dir, hash string, err error) {
	root := e.CacheDir
	if root == "" {
		// No cache is a supported way to run, so this is not an error. The
		// files still have to land somewhere the assembly can copy from.
		tmp, err := os.MkdirTemp("", "gnopm-download-")
		if err != nil {
			return "", "", err
		}
		root = tmp
	}
	dir = downloadPath(root, c.ID, pkgPath)

	if h, err := hashDownloaded(dir); err == nil && h != "" {
		if wantHash == "" || h == wantHash {
			e.tracef("cache    %s %s\n", pkgPath, short(strings.TrimPrefix(h, hashPrefix)))
			return dir, h, nil
		}
		e.logf("warning: the cached copy of %s does not match the hash in %s, refetching\n", pkgPath, lockFile)
	}

	pkg, err := fetchPackage(c, pkgPath)
	if err != nil {
		return "", "", err
	}
	if wantHash != "" && pkg.Hash != wantHash {
		// Not a warning. The lock says these bytes, the chain served other
		// bytes, and on a chain that cannot redefine a path that means the
		// lock is describing a different chain or a different package.
		return "", "", fmt.Errorf("%s on %s hashes to %s, but %s records %s.\n"+
			"  A chain cannot redefine a published path, so this is a different package, not a changed one.\n"+
			"  Check the chain id, or `gnopm get %s` again to record what this chain actually serves",
			pkgPath, c.ID, pkg.Hash, lockFile, wantHash, pkgPath)
	}
	if err := writeDownload(dir, c, pkgPath, pkg); err != nil {
		return "", "", err
	}
	e.tracef("fetched  %s from %s, %d file(s)\n", pkgPath, c.ID, len(pkg.Files))
	return dir, pkg.Hash, nil
}

// writeDownload materializes a fetched package, replacing whatever was there.
func writeDownload(dir string, c *Chain, pkgPath string, pkg fetchedPackage) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, body := range pkg.Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	meta := fmt.Sprintf(""+
		"# Written by gnopm. Safe to delete: `gnopm get` fetches it again.\n"+
		"# The hash in %s is what decides whether a download is right; this file\n"+
		"# is provenance for a human, not a second opinion.\n"+
		"module = %q\nchain = %q\nrpc = %q\nhash = %q\nfetched = %q\nfiles = %q\n",
		lockFile, pkgPath, c.ID, c.RPC, pkg.Hash,
		time.Now().UTC().Format(time.RFC3339), strings.Join(pkg.names(), " "))
	return os.WriteFile(filepath.Join(dir, metaFile), []byte(meta), 0o644)
}

// hashDownloaded hashes what is already in a download directory.
//
// The metadata file is excluded, because gnopm wrote it and it is not part of
// the package. Everything else is included without judgement, which is the same
// rule hashFiles documents for the git-tracked case: deciding which files
// "count" is exactly the judgement call that makes two hashes of one package
// disagree.
func hashDownloaded(dir string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, ent := range ents {
		if ent.IsDir() || ent.Name() == metaFile {
			continue
		}
		names = append(names, ent.Name())
	}
	if len(names) == 0 {
		return "", nil
	}
	return hashDirFiles(dir, names)
}

// copyDownload copies a cached package into the assembly.
//
// A copy rather than a symlink: the assembly is what the toolchain reads, and a
// link into a directory the user may delete, or that lives on another
// filesystem, turns `gnopm clean -cache` into something that can break an
// unrelated workspace's build.
func copyDownload(src, dest string) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, ent := range ents {
		if ent.IsDir() || ent.Name() == metaFile {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, ent.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dest, ent.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fillFromChain materializes every {chain} entry into the assembly.
//
// Called by Install with the entries git cannot produce. Each one goes through
// the shared download cache, so a second workspace on this machine costs no
// chain reads at all, and a first one costs the files once.
func fillFromChain(e *Env, entries []LockEntry, dests []string) error {
	if len(entries) == 0 {
		return nil
	}
	// One discovery per chain, not per package. Discovery is an HTTP round
	// trip to a gnoweb instance, and a workspace pulling ten packages off one
	// chain should pay for it once.
	chains := map[string]*Chain{}
	for i, en := range entries {
		// vendor/ is checked before the cache and before any discovery: a
		// vendored workspace is meant to build with no chain, no cache and no
		// network, and Install has already dropped vendored entries from the
		// list it materializes, so reaching here at all means it is not
		// vendored. Checked anyway, cheaply, because the two must never
		// disagree about what counts as vendored.
		if vendored(e.Root, en.Module, en.Hash) {
			continue
		}
		// The cache answers next, and that is principle 8 rather than an
		// optimisation: a workspace whose dependencies are all already
		// downloaded must complete with no network at all, so discovery does
		// not even happen unless something actually has to be fetched.
		if e.CacheDir != "" && en.Hash != "" {
			cached := downloadPath(e.CacheDir, en.Source.Chain, en.Module)
			if h, err := hashDownloaded(cached); err == nil && h == en.Hash {
				e.tracef("cache    %s, no chain read\n", en.Module)
				if err := copyDownload(cached, dests[i]); err != nil {
					return fmt.Errorf("%s: %w", en.Module, err)
				}
				continue
			}
		}
		c, ok := chains[en.Source.Chain]
		if !ok {
			var err error
			c, err = DiscoverChain(e, en.Module, e.RPC, e.ChainID)
			if err != nil {
				return fmt.Errorf("%s: %w", en.Module, err)
			}
			// The lock names a chain id; discovery names one from the package
			// path. If they disagree, the lock was written against a different
			// network and its hashes describe packages this one has never
			// seen. Failing here beats downloading something else and
			// verifying it against the wrong expectation.
			if c.ID != en.Source.Chain {
				return fmt.Errorf("%s is locked to chain %q, but %s says its chain id is %q.\n"+
					"  A lock written against one network cannot be satisfied by another",
					en.Module, en.Source.Chain, c.Host, c.ID)
			}
			chains[en.Source.Chain] = c
		}
		src, _, err := download(e, c, en.Module, en.Hash)
		if err != nil {
			return err
		}
		if err := copyDownload(src, dests[i]); err != nil {
			return fmt.Errorf("%s: %w", en.Module, err)
		}
	}
	return nil
}

// downloadRoot is where fetched packages live, or "" when there is no cache.
func downloadRoot(cacheRoot string) string {
	if cacheRoot == "" {
		return ""
	}
	return filepath.Join(cacheRoot, downloadSubdir)
}
