package gnopm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reading a chain, with no endpoint table and no gnoclient.
//
// Architecture decision 4: a chain is discoverable from the package path. A
// module path beginning `gno.land/` implies the gnoweb instance at
// https://gno.land, and every gnoweb advertises its own RPC endpoint and chain
// id as meta tags. So "where does this package live" is answered by the path
// itself, and a new chain needs no change here.
//
// Architecture decision 3 is why nothing in this file writes: gnopm reads
// chains freely, and a write is a gnokey command emitted for a human.

// Chain is where a package path lives, and how to talk to it.
type Chain struct {
	// Host is the gnoweb origin the path implies, e.g. https://gno.land.
	Host string
	// RPC is the JSON-RPC endpoint, from gnoconnect:rpc.
	RPC string
	// ID is the chain id, from gnoconnect:chainid.
	ID string
}

// httpClient is shared by every read in this file, and that is the point.
//
// ABCIQuery used to build an http.Client per call, so each of the N package
// lookups a publish plan needs paid a fresh TCP connect and a fresh TLS
// handshake against the same host. Reusing the connection turns a query from a
// round trip plus a handshake into a round trip, which against a public
// endpoint is most of the wall clock.
//
// MaxIdleConnsPerHost is the number that matters here, because every query in
// a run goes to one host: the default of 2 would serialize the concurrent
// probe back down to two connections and give away what the pool just bought.
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        4 * probeConcurrency,
		MaxIdleConnsPerHost: probeConcurrency,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

var (
	rpcMeta     = regexp.MustCompile(`<meta[^>]+name="gnoconnect:rpc"[^>]+content="([^"]+)"`)
	chainIDMeta = regexp.MustCompile(`<meta[^>]+name="gnoconnect:chainid"[^>]+content="([^"]+)"`)

	chainCacheMu sync.Mutex
	chainCache   = map[string]*Chain{}
)

// hostForPath maps a module path onto the gnoweb origin that serves it. The
// first path element is the domain, which is the whole convention: gno package
// paths are domain-prefixed precisely so they say where they belong.
func hostForPath(module string) (string, error) {
	domain := module
	if i := strings.IndexByte(domain, '/'); i >= 0 {
		domain = domain[:i]
	}
	if domain == "" || !strings.Contains(domain, ".") {
		return "", fmt.Errorf("module path %q has no domain to resolve a chain from", module)
	}
	return "https://" + domain, nil
}

// DiscoverChain resolves the chain serving a module path by reading the gnoweb
// instance the path names. Results are cached per host for the process, and
// under e's cache directory for a day: discovery is one HTTPS round trip every
// chain-reading command otherwise pays to be told the same two strings.
//
// An explicit rpc/chainID overrides discovery entirely, for a local gnodev or
// a chain that does not front itself with gnoweb.
func DiscoverChain(e *Env, module, rpc, chainID string) (*Chain, error) {
	if rpc != "" && chainID != "" {
		return &Chain{Host: "", RPC: rpc, ID: chainID}, nil
	}
	host, err := hostForPath(module)
	if err != nil {
		return nil, err
	}

	chainCacheMu.Lock()
	cached, ok := chainCache[host]
	chainCacheMu.Unlock()
	if ok {
		return withOverrides(cached, rpc, chainID), nil
	}

	dir := e.cacheDir()
	if r, id, ok := readDiscovered(dir, host); ok {
		e.tracef("chain    %s is %s (%s), remembered in %s\n", host, id, r, discoveryFile(dir, host))
		c := &Chain{Host: host, RPC: r, ID: id}
		chainCacheMu.Lock()
		chainCache[host] = c
		chainCacheMu.Unlock()
		return withOverrides(c, rpc, chainID), nil
	}

	start := time.Now()
	resp, err := httpClient.Get(host)
	if err != nil {
		return nil, fmt.Errorf("discovering the chain at %s: %w "+
			"(pass -rpc and -chainid to skip discovery)", host, err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	// The tags are in <head>; 64 KiB is generous and bounds a hostile server.
	if _, err := body.ReadFrom(newLimitReader(resp.Body, 64<<10)); err != nil {
		return nil, err
	}
	c := &Chain{Host: host}
	if m := rpcMeta.FindSubmatch(body.Bytes()); m != nil {
		c.RPC = string(m[1])
	}
	if m := chainIDMeta.FindSubmatch(body.Bytes()); m != nil {
		c.ID = string(m[1])
	}
	if c.RPC == "" || c.ID == "" {
		return nil, fmt.Errorf("%s does not advertise gnoconnect:rpc and gnoconnect:chainid "+
			"(pass -rpc and -chainid)", host)
	}
	e.tracef("chain    GET %s said %s (%s) in %s\n", host, c.ID, c.RPC, took(start))
	writeDiscovered(dir, host, c.RPC, c.ID)

	chainCacheMu.Lock()
	chainCache[host] = c
	chainCacheMu.Unlock()
	return withOverrides(c, rpc, chainID), nil
}

func withOverrides(c *Chain, rpc, chainID string) *Chain {
	out := *c
	if rpc != "" {
		out.RPC = rpc
	}
	if chainID != "" {
		out.ID = chainID
	}
	return &out
}

// took renders a duration the way a progress line wants it: short, fixed in
// shape, and never in nanoseconds.
func took(start time.Time) string {
	d := time.Since(start)
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}

// rpcResponse is the slice of the JSON-RPC envelope this tool reads.
type rpcResponse struct {
	Error  *json.RawMessage `json:"error"`
	Result struct {
		Response struct {
			ResponseBase struct {
				Error *struct {
					Type string `json:"@type"`
				} `json:"Error"`
				Data []byte `json:"Data"`
				Log  string `json:"Log"`
			} `json:"ResponseBase"`
		} `json:"response"`
	} `json:"result"`
}

// ABCIQuery is the only network call gnopm makes against a chain: a JSON-RPC
// abci_query, standard library only. `path` is the ABCI path (vm/qfile,
// vm/qinertpaths, vm/qeval, params/...) and `data` its argument.
//
// Data comes back as raw bytes. vm/qeval alone wraps its result in a
// `("…" string)` envelope; unwrapping that is the caller's business, because
// applying it here would turn every good vm/qfile answer into an error.
func (c *Chain) ABCIQuery(path, data string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "abci_query",
		"params": map[string]any{
			"path": path, "data": base64.StdEncoding.EncodeToString([]byte(data)),
			"height": "0", "prove": false,
		},
	})
	if err != nil {
		return "", err
	}
	resp, err := postWithRetry(c.RPC, body)
	if err != nil {
		return "", fmt.Errorf("querying %s: %w", c.RPC, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("querying %s: HTTP %s", c.RPC, resp.Status)
	}
	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding the response from %s: %w", c.RPC, err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("rpc error: %s", string(*out.Error))
	}
	if e := out.Result.Response.ResponseBase.Error; e != nil {
		return "", &ABCIError{Type: e.Type, Log: firstTrace(out.Result.Response.ResponseBase.Log)}
	}
	return string(out.Result.Response.ResponseBase.Data), nil
}

// retrySleep is a variable so a test does not wait out a real backoff.
var retrySleep = time.Sleep

// maxQueryAttempts bounds postWithRetry, and maxQueryDelay caps one wait.
// Eight tries at 1s, 2s, 4s, 8s, 16s, 32s, 60s is about two minutes.
//
// Half a minute was the first guess here and it was measured wrong: a tripped
// https://rpc.gno.land refuses for 66s (2026-10-08, polled every 3s from the
// first 429 to the first 200), so a six-try schedule gives up inside the
// window and reads exactly like no retry at all. Two minutes outlasts it and
// still surfaces an endpoint that is genuinely down inside the same run.
const (
	maxQueryAttempts = 8
	maxQueryDelay    = 60 * time.Second
)

// postWithRetry sends one JSON body, and tries again when the node says "slow
// down" (429) or is briefly unavailable (502, 503, 504), honouring Retry-After
// when it is given in seconds.
//
// A whole-repo scan, `publish -republish` over ~190 live packages, asks the
// public RPC for several answers per package. Without this it died on the first
// 429 with nothing reported (gno-contracts, 2026-10-08), so the one command that
// finds every drifted realm could not be run over all of them.
func postWithRetry(url string, body []byte) (*http.Response, error) {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		pace()
		resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body))
		if err == nil && !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		wait := delay
		if err == nil && resp.StatusCode == http.StatusTooManyRequests {
			slowDown()
		}
		if err == nil {
			if secs, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && secs > 0 && secs <= 60 {
				wait = time.Duration(secs) * time.Second
			}
			if attempt == maxQueryAttempts {
				return resp, nil // the caller reports the status
			}
			resp.Body.Close()
		} else if attempt == maxQueryAttempts {
			return nil, err
		}
		retrySleep(wait)
		if delay *= 2; delay > maxQueryDelay {
			delay = maxQueryDelay
		}
	}
}

// Pacing is adaptive: full speed until a node says 429, then one query per
// throttledSpacing for the rest of the process. Retrying alone does not get a
// whole-repo scan through, because the scan keeps asking at the rate that
// tripped the limit. Starting slow would tax every ordinary publish for the
// sake of the rare scan.
//
// 800ms, because the spacing has to sit under what the endpoint sustains and
// 400ms did not. Measured on https://rpc.gno.land, 2026-10-08: 190 sequential
// queries in 131s before the first 429, so the ceiling is near 90 a minute.
// 400ms asks for 150 a minute, which keeps tripping the limit it was chosen to
// avoid; 800ms asks for 75 and leaves room for whatever else shares the
// address. A whole-repo `-republish` is slow either way, and slow finishing
// beats fast failing.
var (
	paceMu           sync.Mutex
	paceSpacing      time.Duration
	paceLast         time.Time
	throttledSpacing = 800 * time.Millisecond
)

func slowDown() {
	paceMu.Lock()
	defer paceMu.Unlock()
	paceSpacing = throttledSpacing
}

func pace() {
	paceMu.Lock()
	wait := time.Duration(0)
	if paceSpacing > 0 {
		if since := time.Since(paceLast); since < paceSpacing {
			wait = paceSpacing - since
		}
	}
	paceLast = time.Now().Add(wait)
	paceMu.Unlock()
	if wait > 0 {
		retrySleep(wait)
	}
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// ABCIError is an answer, not a failure: the query reached a node and the node
// said no.
//
// It exists to be distinguishable from a transport error, and the distinction
// is load-bearing wherever a chain answer gates a decision. "this chain does
// not have that package" and "I could not reach that chain" must never land in
// the same branch, or an unreachable node reads as an empty one and every
// guard built on "it was never published" silently opens.
type ABCIError struct {
	Type string
	Log  string
}

func (e *ABCIError) Error() string {
	if e.Log == "" {
		return e.Type
	}
	return e.Type + ": " + e.Log
}

// answered reports whether err is the chain saying no rather than the network
// failing to ask.
func answered(err error) bool {
	var ae *ABCIError
	return errors.As(err, &ae)
}

// firstTrace pulls the human-readable cause out of an ABCI error log, whose
// first line is a generic banner.
func firstTrace(log string) string {
	for _, line := range strings.Split(log, "\n") {
		if i := strings.Index(line, " - "); i >= 0 {
			return strings.TrimSpace(line[i+3:])
		}
	}
	return strings.TrimSpace(log)
}
