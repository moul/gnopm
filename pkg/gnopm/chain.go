package gnopm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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

// DiscoverChain resolves the chain serving a module path by reading the
// gnoweb instance the path names. Results are cached per host for the process.
//
// An explicit rpc/chainID overrides discovery entirely, for a local gnodev or
// a chain that does not front itself with gnoweb.
func DiscoverChain(module, rpc, chainID string) (*Chain, error) {
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
	resp, err := httpClient.Post(c.RPC, "application/json", bytes.NewReader(body))
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
