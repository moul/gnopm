package gnopm

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	for _, tc := range []struct {
		remote, want string
	}{
		{"https://github.com/moul/gnopm.git", "https://github.com/moul/gnopm"},
		{"https://github.com/moul/gnopm/", "https://github.com/moul/gnopm"},
		{"git@github.com:moul/gnopm.git", "https://github.com/moul/gnopm"},
		{"ssh://git@github.com:22/moul/gnopm.git", "https://github.com/moul/gnopm"},
		{"https://x-access-token:ghs_secret@github.com/moul/gnopm.git\n", "https://github.com/moul/gnopm"},
		{"git@gitlab.com:group/sub/repo.git", "https://gitlab.com/group/sub/repo"},
		{"/srv/git/repo.git", ""},
		{"file:///srv/git/repo.git", ""},
		{"https://github.com", ""},
	} {
		got, err := normalizeRemote(tc.remote)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%q: got %q, want an error", tc.remote, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.remote, got, err, tc.want)
		}
	}
}

func TestSourceSectionValidate(t *testing.T) {
	ok := sourceSection{Repository: "https://github.com/moul/gnopm", Path: "r/app", Revision: "3cc494ec4"}
	for _, tc := range []struct {
		name string
		s    sourceSection
		bad  bool
	}{
		{"full", ok, false},
		{"no repository", sourceSection{Path: "r/app"}, true},
		{"http", sourceSection{Repository: "http://github.com/moul/gnopm"}, true},
		{"javascript", sourceSection{Repository: "javascript:alert(1)"}, true},
		{"credentials", sourceSection{Repository: "https://tok@github.com/moul/gnopm"}, true},
		{"query", sourceSection{Repository: "https://github.com/moul/gnopm?x=1"}, true},
		{"dotdot", sourceSection{Repository: ok.Repository, Path: "../x"}, true},
		{"absolute", sourceSection{Repository: ok.Repository, Path: "/x"}, true},
		{"branch as revision", sourceSection{Repository: ok.Repository, Revision: "main"}, true},
		{"too long", sourceSection{Repository: ok.Repository + "/" + strings.Repeat("a", 300)}, true},
	} {
		if err := tc.s.validate(); (err != nil) != tc.bad {
			t.Errorf("%s: validate() = %v, want error %v", tc.name, err, tc.bad)
		}
	}
}

func TestWithSource(t *testing.T) {
	s := sourceSection{Repository: "https://github.com/moul/gnopm", Path: "r/app", Revision: "3cc494ec4"}
	const stanza = "[source]\n  repository = \"https://github.com/moul/gnopm\"\n  path = \"r/app\"\n  revision = \"3cc494ec4\"\n"
	for _, tc := range []struct {
		name, in string
		s        sourceSection
		want     string
		bad      bool
	}{
		{name: "appended", in: "module = \"a\"\ngno = \"0.9\"\n", s: s,
			want: "module = \"a\"\ngno = \"0.9\"\n\n" + stanza},
		{name: "declared one replaced", in: "module = \"a\"\n\n[source]\n# where\nrepository = \"https://x.org/o/r\"\nrevision = \"aaaaaaa\"\n", s: s,
			want: "module = \"a\"\n\n" + stanza},
		{name: "table after it kept", in: "module = \"a\"\n[source]\nrepository = \"https://x.org/o/r\"\n[[replace]]\nold = \"x\"\nnew = \"y\"\n", s: s,
			want: "module = \"a\"\n[[replace]]\nold = \"x\"\nnew = \"y\"\n\n" + stanza},
		{name: "zero leaves a file without one alone", in: "module = \"a\"\n", want: "module = \"a\"\n"},
		{name: "zero drops a declared one", in: "module = \"a\"\n[source]\nrepository = \"https://x.org/o/r\"\n", want: "module = \"a\"\n"},
		{name: "unreadable value", in: "module = \"a\"\n[source]\nrepository = 'https://x.org/o/r'\n", s: s, bad: true},
	} {
		got, err := withSource(tc.in, tc.s)
		if tc.bad {
			if err == nil {
				t.Errorf("%s: got %q, want an error", tc.name, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s:\n got %q, %v\nwant %q", tc.name, got, err, tc.want)
		}
	}
}

// sourceRepo is a one-package workspace whose origin is on GitHub.
func sourceRepo(t *testing.T, gnomod string) string {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "r/moul/app", "gno.land/r/moul/app/v0", "package app\n\nfunc A() string { return \"a\" }\n")
	if gnomod != "" {
		write(t, filepath.Join(root, "r/moul/app/gnomod.toml"), gnomod)
	}
	gitCmd(t, root, "remote", "add", "origin", "git@github.com:moul/contracts.git")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	return root
}

func withPublic(t *testing.T, public bool) *[]string {
	t.Helper()
	var asked []string
	prev := repoIsPublic
	t.Cleanup(func() { repoIsPublic = prev })
	repoIsPublic = func(repo string) (bool, error) {
		asked = append(asked, repo)
		return public, nil
	}
	return &asked
}

func TestSourceResolver(t *testing.T) {
	const declared = "module = \"gno.land/r/moul/app/v0\"\ngno = \"0.9\"\n\n[source]\n  repository = \"https://github.com/moul/mirror\"\n"
	for _, tc := range []struct {
		name     string
		gnomod   string
		public   bool
		disabled bool
		dirty    bool
		want     sourceSection // Revision "HEAD" means: the commit at HEAD
	}{
		{name: "public origin", public: true,
			want: sourceSection{Repository: "https://github.com/moul/contracts", Path: "r/moul/app", Revision: "HEAD"}},
		// The case the whole public check exists for: a private repository's
		// name must never reach the chain on its own.
		{name: "private origin", public: false},
		{name: "declared wins over a private origin", gnomod: declared, public: false,
			want: sourceSection{Repository: "https://github.com/moul/mirror", Path: "r/moul/app", Revision: "HEAD"}},
		{name: "dirty package has no revision", public: true, dirty: true,
			want: sourceSection{Repository: "https://github.com/moul/contracts", Path: "r/moul/app"}},
		{name: "disabled", public: true, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := sourceRepo(t, tc.gnomod)
			withPublic(t, tc.public)
			if tc.dirty {
				write(t, filepath.Join(root, "r/moul/app/app.gno"), "package app\n\n// edited\n")
			}
			disk, err := os.ReadFile(filepath.Join(root, "r/moul/app/gnomod.toml"))
			if err != nil {
				t.Fatal(err)
			}
			r := newSourceResolver(testEnv(root, &bytes.Buffer{}), tc.disabled)
			got, err := r.section("r/moul/app", string(disk))
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want.Revision == "HEAD" {
				head, err := gitHead(root)
				if err != nil {
					t.Fatal(err)
				}
				want.Revision = head
			}
			if got != want {
				t.Fatalf("got %+v\nwant %+v", got, want)
			}
		})
	}
}

// TestPublishStampsSourceOnTheUploadedCopyOnly: the section is in what goes up,
// the bytes it adds are in what gas and deposit were sized from, and the file
// in the working tree is untouched.
func TestPublishStampsSourceOnTheUploadedCopyOnly(t *testing.T) {
	root := sourceRepo(t, "")
	asked := withPublic(t, true)
	gnomodPath := filepath.Join(root, "r/moul/app/gnomod.toml")
	before, err := os.ReadFile(gnomodPath)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t)
	out := filepath.Join(t.TempDir(), "tx.json")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"publish", "-print", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1",
		"-key", "moul", "-addr", testCreator, "-o", out}, &stdout, &stderr); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr.String())
	}
	if len(*asked) != 1 {
		t.Errorf("asked whether the repository is public %d times, want once per run", len(*asked))
	}

	var doc TxDocument
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	head, _ := gitHead(root)
	var uploaded string
	for _, f := range doc.Msgs[0].Package.Files {
		if f.Name == "gnomod.toml" {
			uploaded = f.Body
		}
	}
	for _, want := range []string{
		"[source]",
		`repository = "https://github.com/moul/contracts"`,
		`path = "r/moul/app"`,
		`revision = "` + head + `"`,
	} {
		if !strings.Contains(uploaded, want) {
			t.Errorf("uploaded gnomod.toml lacks %s:\n%s", want, uploaded)
		}
	}

	after, err := os.ReadFile(gnomodPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("publish wrote the working tree:\n%s", after)
	}
}

// TestStampSourceMovesTheByteCount: gas and the deposit ceiling are sized from
// pl.bytes, so the section has to be counted there. Asserted on the plan rather
// than on the document, because DepositFor rounds up to whole GNOT with a floor
// of five: a hundred-odd bytes never moves it for a small package, and an
// assertion on max_deposit stayed green with the count deliberately left out.
func TestStampSourceMovesTheByteCount(t *testing.T) {
	root := sourceRepo(t, "")
	withPublic(t, true)
	files, n, err := Payload(filepath.Join(root, "r/moul/app"))
	if err != nil {
		t.Fatal(err)
	}
	pl := plan{pkg: Package{Dir: "r/moul/app", Module: "gno.land/r/moul/app/v0"}, bytes: n, files: files}
	if err := pl.stampSource(root, newSourceResolver(testEnv(root, &bytes.Buffer{}), false)); err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(filepath.Join(root, "r/moul/app/gnomod.toml"))
	if err != nil {
		t.Fatal(err)
	}
	delta := len(pl.gnomod) - len(disk)
	if delta <= 0 {
		t.Fatalf("nothing was stamped: %q", pl.gnomod)
	}
	if pl.bytes != n+delta {
		t.Errorf("bytes %d, want %d: the section is uploaded but not paid for", pl.bytes, n+delta)
	}
	for _, f := range pl.files {
		if f.Name == "gnomod.toml" && f.Size != len(pl.gnomod) {
			t.Errorf("report says gnomod.toml is %d bytes, it uploads %d", f.Size, len(pl.gnomod))
		}
	}
}

// The chain stores the section, the tree never has it, and the revision moves
// with every commit: none of that may read as a change worth a redeploy.
func TestNormalizeGnomodIgnoresSource(t *testing.T) {
	tree := "module = \"gno.land/r/moul/app/v0\"\ngno = \"0.9\"\n"
	chain := tree + "\n[source]\n  repository = \"https://github.com/moul/contracts\"\n  revision = \"3cc494ec4\"\n\n[addpkg]\n  creator = \"g1x\"\n  height = 1\n"
	if normalizeGnomod(tree) != normalizeGnomod(chain) {
		t.Fatalf("a [source] section on chain reads as a change:\n%q\n%q", normalizeGnomod(tree), normalizeGnomod(chain))
	}
}
