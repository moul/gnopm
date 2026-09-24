package gnopm

import "moul.io/gnopm/pkg/gnomodlock"

// The gnomod.lock format lives in its own package because more than one
// program reads it: the catalog tooling has to know about versions that no
// longer have a directory. These aliases keep gnopm's own code reading in
// terms of the thing it manipulates rather than in terms of the import path.
type (
	Lock      = gnomodlock.Lock
	LockEntry = gnomodlock.LockEntry
	Source    = gnomodlock.Source
)

const (
	lockFile   = gnomodlock.LockFile
	lockFormat = gnomodlock.FormatVersion
	// gnomodFile is the manifest gno itself reads. Named here because
	// orphan.go asks git for one by path at a commit, where a literal would
	// be a second place to change it.
	gnomodFile = "gnomod.toml"
)

var (
	readLock  = gnomodlock.Read
	writeLock = gnomodlock.Write
	parseLock = gnomodlock.Parse
	unquote   = gnomodlock.Unquote
)
