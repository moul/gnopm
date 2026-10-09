package gnopm

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// Answering "which live realms drifted" without reading the whole chain again.
//
// checkRepublish is cheap per package and ruinous per workspace. Comparing one
// package is a read of its gnomod.toml, a read of its file list, then one per
// file, and publishcmd ran them in a serial loop. On moul/gno-contracts, 282
// live packages, that measured **1269s end to end** against rpc.gno.land, most
// of it waiting out the rate limit those same reads tripped (gnopm#88).
//
// Three observations cut it to a handful of reads, and each one is a fact
// about the chain rather than a guess:
//
//  1. `private` is permanent (cache.go says why, at length). 227 of those 282
//     answer "public, so it can never be republished", and that answer is worth
//     exactly one read ever, not one per run.
//  2. The chain's copy of a package changes only when its creator redeploys it,
//     and `vm/qstorage` is one read that moves when it does. So the file bodies
//     behind an unchanged storage figure are the ones already on disk.
//  3. Nothing about one package's answer touches another's, so the scan is a
//     batch, exactly like Probe.Warm.
type republishScanner struct {
	chain   *Chain
	root    string
	docs    bool
	private *privateCache
	source  *sourceCache

	// storageMu guards storageOff, which latches when a chain turns out not to
	// answer vm/qstorage at all. Without it every package on such a chain pays
	// a wasted read to learn the same thing.
	storageMu  sync.Mutex
	storageOff bool
}

// scanConcurrency is how many packages are compared at once.
//
// Twice probeConcurrency, because the two batches are not the same shape. A
// warm scan is one vm/qstorage per package and nothing else, so its wall clock
// is round trips and only round trips; the cold one behind it is bounded by
// the endpoint's rate limit either way, and chain.go's pacing is what holds
// that line rather than this number.
const scanConcurrency = 2 * probeConcurrency

// scan fills in each entry's republish verdict, in parallel.
//
// The error is the first transport failure, and the batch stops asking once it
// has one, matching Probe.Warm: an unreachable node must never be reported as
// a workspace with nothing to republish.
func (s *republishScanner) scan(targets []*plan) error {
	if len(targets) == 0 {
		return nil
	}
	workers := scanConcurrency
	if len(targets) < workers {
		workers = len(targets)
	}
	work := make(chan *plan)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pl := range work {
				errMu.Lock()
				stop := firstErr != nil
				errMu.Unlock()
				if stop {
					continue // drain, so the producer is never left blocked
				}
				chk, err := s.check(pl.pkg)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					continue
				}
				pl.changed = chk.changed
				if chk.eligible {
					pl.republish = true
				} else {
					pl.skipped = chk.why
				}
			}
		}()
	}
	for _, pl := range targets {
		work <- pl
	}
	close(work)
	wg.Wait()
	return firstErr
}

// chainPrivateCached answers the permanent question once per chain, ever.
func (s *republishScanner) chainPrivateCached(module string) (private, ok bool, err error) {
	if st, hit := s.private.lookup(module); hit {
		return st == privatePrivate, st != privateAbsent, nil
	}
	private, ok, err = chainPrivate(s.chain, module)
	if err != nil {
		return false, false, err
	}
	switch {
	case !ok:
		s.private.learn(module, privateAbsent)
	case private:
		s.private.learn(module, privatePrivate)
	default:
		s.private.learn(module, privatePublic)
	}
	return private, ok, nil
}

// chainSourceHashes is the chain's copy of a package reduced to what the
// comparison actually reads: one hash per file, of the normalized body.
//
// One vm/qstorage read decides whether the copy on disk is still the chain's.
// A chain that does not answer that query, or a path it does not know, falls
// straight through to reading every file, which is what this did before.
func (s *republishScanner) chainSourceHashes(module string) (map[string]string, bool, error) {
	storage, err := s.chainStorage(module)
	if err != nil {
		return nil, false, err
	}
	if storage != "" {
		if files, hit := s.source.lookup(module, storage); hit {
			return files, true, nil
		}
	}
	files, err := chainSource(s.chain, module)
	if err != nil {
		return nil, false, err
	}
	hashed := hashSource(files)
	if storage != "" {
		s.source.learn(module, storage, hashed)
	}
	return hashed, false, nil
}

// chainStorage reads vm/qstorage, and "" means "no usable token, read the
// files". A chain answer is not a failure here: an older node has no such
// query, and the first one to say so turns the read off for the rest of the
// run rather than paying it once per package.
func (s *republishScanner) chainStorage(module string) (string, error) {
	s.storageMu.Lock()
	off := s.storageOff
	s.storageMu.Unlock()
	if off {
		return "", nil
	}
	raw, err := s.chain.ABCIQuery("vm/qstorage", module)
	if err != nil {
		if answered(err) {
			// Could be this one path, could be the whole query. Latching on
			// the first no is wrong only in the direction of reading more.
			s.storageMu.Lock()
			s.storageOff = true
			s.storageMu.Unlock()
			return "", nil
		}
		return "", err
	}
	// Whitespace out, because the token is written into a space-separated
	// cache line and read back from it: "storage: 1, deposit: 2" and
	// "storage:1,deposit:2" have to be the same token or every lookup misses,
	// which is exactly what the first version of this did.
	return strings.Join(strings.Fields(raw), ""), nil
}

// hashSource reduces a file map to name -> hash of the normalized body.
//
// Normalizing before hashing is not an optimisation, it is the comparison:
// the chain rewrites gnomod.toml rather than storing what it was sent, so a
// raw hash marks that file as changed forever (see gnomodnorm.go).
func hashSource(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for name, body := range files {
		out[name] = hashNormalized(name, body)
	}
	return out
}

func hashNormalized(name, body string) string {
	sum := sha256.Sum256([]byte(normalizeForCompare(name, body)))
	return hex.EncodeToString(sum[:])
}
