package gnopm

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// What gnopm remembers between runs, and why it is only ever one thing.
//
// `gnopm publish` over a workspace with two hundred packages is two hundred
// blocking vm/qfile reads, and nearly every one re-establishes a fact that
// cannot change. A package path that is live on a chain stays live: there is no
// delete, and addpkg on an occupied path fails, so its bytes can never be
// redefined either. That makes "live" a permanent property of (chain id, path),
// and the only chain answer worth keeping on disk.
//
// Nothing else qualifies. Parked is a submission still waiting on an approver,
// so it can still be enabled or rejected. Absent is the state of the very
// version you are about to publish, which means a negative cache would be wrong
// exactly when it was consulted. Caching either would make gnopm confidently
// wrong about the one thing it is asked.

const (
	// cacheEnv names the cache directory, and switches it off.
	cacheEnv = "GNOPM_CACHE"

	// cacheDirName is where the cache lives when nothing says otherwise.
	//
	// Deliberately not under GNOHOME: this is gnopm's own memory of chains it
	// has read, it is safe to delete at any moment, and putting it inside the
	// toolchain's home would make "remove it and try again" sound riskier than
	// it is.
	cacheDirName = ".gnopm"

	// devChainID is gnodev's default chain id, for both `gnodev local` and
	// `gnodev staging` (contribs/gnodev/command_local.go, command_staging.go).
	// A gnodev restart wipes the chain and reuses the id, so a cache keyed on
	// it would claim packages that no longer exist. Never persisting it is
	// cheaper and more honest than any staleness check could be.
	devChainID = "dev"

	// discoveryTTL bounds how long a discovered rpc and chain id are reused.
	//
	// Discovery is one HTTPS round trip that every chain-reading command pays
	// to be told the same two strings, and worth removing. But a chain id does
	// change, for a new network or a renamed one, and a stale one would put a
	// wrong -chainid in an emitted gnokey command. That fails at the node
	// rather than doing damage, so a day is a fair trade, and -no-cache skips
	// it outright.
	discoveryTTL = 24 * time.Hour
)

// cacheDir resolves where the cache lives. "" means no cache at all: an
// explicit off, or a machine with no home directory to put one in.
//
// A cache is an optimisation, so every failure to find one is silent and costs
// a chain read. None of them is an error.
func cacheDir() string {
	switch v := strings.TrimSpace(os.Getenv(cacheEnv)); strings.ToLower(v) {
	case "":
	case "off", "0", "no", "none", "false":
		return ""
	default:
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, cacheDirName)
}

// liveCache is the set of paths one chain has already said are live.
//
// The file is one path per line and is only ever appended to. Two gnopm runs
// against the same chain therefore cannot clobber each other, and a write torn
// by a crash leaves a partial line that matches no package path, which is a
// cache miss rather than a wrong answer. Rewriting a whole document on every
// run would give up both properties to save a few kilobytes.
type liveCache struct {
	// file is where it persists. "" is a cache that answers from memory and
	// writes nothing: no cache directory, or a chain whose id must not be
	// trusted between runs.
	file string

	mu   sync.Mutex
	live map[string]bool
	// start is how many paths the file already held, so -v can say what the
	// cache saved rather than only what it knows.
	start int
	// learned counts the paths this run discovered and wrote, which is what
	// the next run will be faster by.
	learned int
	// hits counts the reads answered without touching the chain.
	hits int
}

// openLiveCache loads the cache for one chain. It never fails: an unreadable or
// absent file is an empty cache, and an empty cache is correct, just slower.
func openLiveCache(dir, chainID string) *liveCache {
	c := &liveCache{live: map[string]bool{}}
	if dir == "" || chainID == "" || chainID == devChainID {
		return c
	}
	name := cacheFileName(chainID)
	if name == "" {
		return c
	}
	c.file = filepath.Join(dir, "live", name)
	b, err := os.ReadFile(c.file)
	if err != nil {
		return c
	}
	for _, line := range strings.Split(string(b), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			c.live[p] = true
		}
	}
	c.start = len(c.live)
	return c
}

// cacheFileName turns a chain id into one safe file name. The id arrives from a
// meta tag on a remote page, so it is not ours to trust with a path: anything
// outside the allowed set becomes an underscore, and an id that is only
// separators gets no file at all.
func cacheFileName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		return ""
	}
	return name
}

// has reports whether this chain has already been seen serving that path, and
// counts the read as a hit.
func (c *liveCache) has(path string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live[path] {
		c.hits++
		return true
	}
	return false
}

// learn records a path the chain just said is live, and appends it straight
// away.
//
// Writing now rather than at the end costs one short append against a round
// trip that already took a fifth of a second, and in exchange a run killed
// half way through keeps everything it learned. A flush at the end is the
// version somebody forgets to call.
func (c *liveCache) learn(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live[path] {
		return
	}
	c.live[path] = true
	c.learned++
	if c.file == "" {
		return
	}
	// A cache that cannot be written is still a correct cache, so every
	// failure here disables persistence for the rest of the run and says
	// nothing. Warning about it on every path would bury the report.
	if err := os.MkdirAll(filepath.Dir(c.file), 0o755); err != nil {
		c.file = ""
		return
	}
	f, err := os.OpenFile(c.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		c.file = ""
		return
	}
	defer f.Close()
	if _, err := f.WriteString(path + "\n"); err != nil {
		c.file = ""
	}
}

// stats reports what the cache held on entry, what it answered, and what it
// learned, which is the whole of what -v has to say about it.
func (c *liveCache) stats() (start, hits, learned int) {
	if c == nil {
		return 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start, c.hits, c.learned
}

// where names the file, for `gnopm env` and for -v. "" when nothing persists.
func (c *liveCache) where() string {
	if c == nil {
		return ""
	}
	return c.file
}

// readDiscovered is the cached answer to "what rpc and chain id does this
// gnoweb advertise": two lines, rpc then chain id, aged by the file's mtime.
func readDiscovered(dir, host string) (rpc, chainID string, ok bool) {
	p := discoveryFile(dir, host)
	if p == "" {
		return "", "", false
	}
	st, err := os.Stat(p)
	if err != nil || time.Since(st.ModTime()) > discoveryTTL {
		return "", "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		return "", "", false
	}
	rpc, chainID = strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	if rpc == "" || chainID == "" {
		return "", "", false
	}
	return rpc, chainID, true
}

func writeDiscovered(dir, host, rpc, chainID string) {
	p := discoveryFile(dir, host)
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	// Whole-file replace, not append: unlike the live set this is one value
	// that gets superseded, and the file's own mtime is its expiry.
	_ = os.WriteFile(p, []byte(rpc+"\n"+chainID+"\n"), 0o644)
}

func discoveryFile(dir, host string) string {
	if dir == "" || host == "" {
		return ""
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	name := cacheFileName(host)
	if name == "" {
		return ""
	}
	return filepath.Join(dir, "hosts", name)
}
