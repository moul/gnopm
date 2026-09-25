// Command fakechain answers the two ABCI queries `gnopm publish` makes, and
// nothing else.
//
// It exists so scripts/screenshots.sh can capture `publish`, which is the
// command most worth showing and the only one that reads a chain. The rule it
// has to respect is the one that makes the demo a real integration test: no
// network. A capture taken once against mainnet and committed would be wrong
// the first time the report's wording changed, which is exactly what
// CONTRIBUTING forbids.
//
// It is deliberately not a chain. It knows two answers:
//
//	vm/qinertpaths              the parked paths, as one string
//	vm/qfile <path>             ok if the path is live, an ABCI error if not
//	vm/qfile <path>/gnomod.toml the gnomod that path was published with
//
// Anything else 404s rather than guessing, because a helper that invents an
// answer to a query gnopm did not expect would make a capture that documents a
// chain nobody has.
//
// usage:
//
//	go run ./scripts/fakechain -live a,b -parked c   # prints its URL, serves until killed
//	go run ./scripts/fakechain -live a -private a    # a's chain copy declares private
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	live := flag.String("live", "", "comma-separated package paths this chain has published")
	serve := flag.String("serve", "", "comma-separated <pkgpath>=<dir> pairs served as real source")
	parked := flag.String("parked", "", "comma-separated package paths submitted but not enabled")
	private := flag.String("private", "", "comma-separated live paths whose published gnomod declares private = true")
	addr := flag.String("addr", "127.0.0.1:0", "listen address; port 0 picks a free one")
	flag.Parse()

	served, err := parseServed(*serve)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakechain:", err)
		os.Exit(2)
	}
	srv := &server{live: set(*live), parked: split(*parked), private: set(*private), served: served}

	// Listen before printing, so the URL on stdout is a promise: a caller that
	// reads a line and immediately connects cannot lose the race.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakechain:", err)
		os.Exit(1)
	}
	fmt.Printf("http://%s\n", ln.Addr().String())
	_ = os.Stdout.Sync()
	if err := http.Serve(ln, srv); err != nil {
		fmt.Fprintln(os.Stderr, "fakechain:", err)
		os.Exit(1)
	}
}

type server struct {
	live    map[string]bool
	parked  []string
	private map[string]bool
	// served maps a package path to a directory whose files are that
	// package's real source. -live fakes a listing, which is all `publish`
	// looks at; -serve answers with actual bytes, which is what `gnopm get`
	// needs in order to fetch anything.
	served map[string]string
}

// parseServed reads the -serve pairs, and checks each directory now rather
// than on the first query: a typo should stop the script that started this,
// not produce an empty package three steps later.
func parseServed(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range split(s) {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("-serve wants <pkgpath>=<dir>, got %q", pair)
		}
		if _, err := os.ReadDir(v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// fileNames lists a served package's plain files, sorted, as the chain does.
func fileNames(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// request is only the part of the JSON-RPC envelope this needs. Decoding into
// the full shape would mean keeping a second copy of tm2's types here.
type request struct {
	Params struct {
		Path string `json:"path"`
		Data string `json:"data"` // base64, as gnopm sends it
	} `json:"params"`
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	arg, err := base64.StdEncoding.DecodeString(req.Params.Data)
	if err != nil {
		http.Error(w, "bad data", http.StatusBadRequest)
		return
	}
	switch req.Params.Path {
	case "vm/qinertpaths":
		reply(w, []byte(strings.Join(s.parked, "\n")), nil)
	case "vm/qfile":
		// A served package answers both shapes for real, and is checked first
		// so -serve wins over -live for the same path.
		target := string(arg)
		if dir, ok := s.served[target]; ok {
			names, err := fileNames(dir)
			if err != nil {
				reply(w, nil, &abciError{Type: "/std.InternalError"})
				return
			}
			reply(w, []byte(strings.Join(names, "\n")), nil)
			return
		}
		if i := strings.LastIndex(target, "/"); i > 0 {
			if dir, ok := s.served[target[:i]]; ok {
				b, err := os.ReadFile(filepath.Join(dir, filepath.Base(target)))
				if err != nil {
					reply(w, nil, &abciError{Type: "/vm.InvalidFileError"})
					return
				}
				reply(w, b, nil)
				return
			}
		}
		// A package path lists its files; publish reads only whether the
		// query succeeded. `<path>/gnomod.toml` is the one body that is read
		// rather than counted, because publish compares the `private` the
		// chain holds against the one in the working tree.
		if path, ok := strings.CutSuffix(target, "/gnomod.toml"); ok {
			if s.live[path] {
				reply(w, []byte(gnomodOf(path, s.private[path])), nil)
				return
			}
		} else if s.live[target] {
			reply(w, []byte("gnomod.toml\n"), nil)
			return
		}
		// An absent path is the node saying no, not the network failing. gnopm
		// distinguishes the two deliberately, and every guard it has is built
		// on that, so this has to be a real ABCI error.
		reply(w, nil, &abciError{Type: "/std.UnknownRequestError"})
	default:
		http.Error(w, "fakechain does not answer "+req.Params.Path, http.StatusNotFound)
	}
}

// gnomodOf is the gnomod.toml a path was published with. Only the two lines
// anything reads: publish parses the module path from neither and the private
// flag from this.
func gnomodOf(path string, private bool) string {
	out := "module = \"" + path + "\"\ngno = \"0.9\"\n"
	if private {
		out += "private = true\n"
	}
	return out
}

type abciError struct {
	Type string `json:"@type"`
}

func reply(w http.ResponseWriter, data []byte, abci *abciError) {
	var base struct {
		Error *abciError `json:"Error"`
		Data  []byte     `json:"Data"`
		Log   string     `json:"Log"`
	}
	base.Error, base.Data = abci, data
	if abci != nil {
		base.Log = "package does not exist"
	}
	out := map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{"response": map[string]any{"ResponseBase": base}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func split(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := strings.Split(s, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func set(s string) map[string]bool {
	m := map[string]bool{}
	for _, v := range split(s) {
		m[v] = true
	}
	return m
}
