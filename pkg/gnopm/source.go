package gnopm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The [source] section of gnomod.toml, managed lazily.
//
// gno lets a package say where its code lives (gnolang/gno#6282):
//
//	[source]
//	  repository = "https://github.com/someone/contracts"
//	  path = "r/app"
//	  revision = "3cc494ec4d1f…"
//
// Nobody should write that by hand, and nobody should commit it: a committed
// revision is stale one commit later, and the path is a fact git already knows.
// So gnopm writes it at publish time, into the copy of gnomod.toml it uploads,
// and never into the working tree. The file on disk stays what the author wrote.
//
// gnokey stays out of this on purpose. It signs what it is given; deciding what
// a package says about itself is the package manager's job.
//
// Three rules decide what goes up:
//
//  1. Declared wins. A [source] table already in the file is the author's
//     word: its repository is kept as written, and only a missing path, and
//     the revision, are filled in.
//  2. Otherwise only a public repository is named. The repository comes from
//     the git origin remote, and it is put on chain only when GitHub says, to
//     an unauthenticated request, that it is public. A private repository's
//     name on chain is a leak no redeploy can take back, so anything short of
//     that answer (another host, no network, a rate limit) means no section.
//  3. A revision only for a clean package. If the package directory has
//     uncommitted changes, HEAD does not describe what is being uploaded, and
//     a revision that lies is worse than none: the section goes up without one.
//
// The chain rewrites gnomod.toml, so the republish comparison ignores [source]
// entirely (gnomodnorm.go): a new commit alone is not a reason to redeploy.

// sourceSection is one [source] table.
type sourceSection struct {
	Repository, Path, Revision string
}

func (s sourceSection) zero() bool { return s == sourceSection{} }

// render writes the table the way gno's own encoder does, so what the chain
// stores back is what was sent.
func (s sourceSection) render() string {
	var b strings.Builder
	b.WriteString("[source]\n")
	fmt.Fprintf(&b, "  repository = %q\n", s.Repository)
	if s.Path != "" {
		fmt.Fprintf(&b, "  path = %q\n", s.Path)
	}
	if s.Revision != "" {
		fmt.Fprintf(&b, "  revision = %q\n", s.Revision)
	}
	return b.String()
}

// maxSourceField mirrors maxSourceFieldLen in gnovm/pkg/gnomod/file.go
// (gnolang/gno#6282, read 2026-10-06), as does the rest of validate.
const maxSourceField = 256

var hexRevision = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// validate mirrors gnomod.Source.Validate (gnolang/gno#6282, read 2026-10-06).
// A chain that has the rule refuses the whole package over a bad section, so a
// declared one that would fail is reported here, before anything is signed.
func (s sourceSection) validate() error {
	if s.Repository == "" {
		return fmt.Errorf("[source] has no repository")
	}
	for name, v := range map[string]string{"repository": s.Repository, "path": s.Path, "revision": s.Revision} {
		if len(v) > maxSourceField {
			return fmt.Errorf("[source] %s is longer than %d bytes", name, maxSourceField)
		}
		if strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("[source] %s contains a control character", name)
		}
	}
	u, err := url.Parse(s.Repository)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(s.Repository, "?#") {
		return fmt.Errorf("[source] repository %q must be a plain https URL, with no credentials, query or fragment", s.Repository)
	}
	if p := s.Path; p != "" && (strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../")) {
		return fmt.Errorf("[source] path %q must be a clean relative path", p)
	}
	if s.Revision != "" && !hexRevision.MatchString(s.Revision) {
		return fmt.Errorf("[source] revision %q must be a lowercase hex commit id", s.Revision)
	}
	return nil
}

var (
	tableHeader = regexp.MustCompile(`^\s*\[`)
	sourceKey   = regexp.MustCompile(`^\s*(repository|path|revision)\s*=\s*"([^"\\]*)"\s*(#.*)?$`)
)

// splitSource cuts the [source] table out of a gnomod.toml body and returns
// the rest, plus what the table declared.
//
// Line-based, like normalizeGnomod, and for the same reason: gnopm has no TOML
// parser and does not want one. The table is everything from its header to
// the next header. A value this cannot read (an escape, a multi-line string)
// is an error rather than a guess, because the alternative is uploading a
// section the author did not write.
func splitSource(body string) (rest string, declared sourceSection, found bool, err error) {
	var out []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if tableHeader.MatchString(line) {
			in = t == "[source]"
			if in {
				found = true
				continue
			}
		}
		if !in {
			out = append(out, line)
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		m := sourceKey.FindStringSubmatch(line)
		if m == nil {
			return "", sourceSection{}, false, fmt.Errorf("cannot read %q in [source]: write it as key = \"value\"", t)
		}
		switch m[1] {
		case "repository":
			declared.Repository = m[2]
		case "path":
			declared.Path = m[2]
		case "revision":
			declared.Revision = m[2]
		}
	}
	return strings.Join(out, "\n"), declared, found, nil
}

// withSource returns body with its [source] table replaced by s, appended
// last, or body unchanged when s is zero.
func withSource(body string, s sourceSection) (string, error) {
	rest, _, found, err := splitSource(body)
	if err != nil {
		return "", err
	}
	if s.zero() {
		if found {
			return strings.TrimRight(rest, "\n") + "\n", nil
		}
		return body, nil
	}
	return strings.TrimRight(rest, "\n") + "\n\n" + s.render(), nil
}

// sourceResolver works out the section for each package of one publish run.
// Git and GitHub are asked once per run, not once per package.
type sourceResolver struct {
	e        *Env
	disabled bool

	once     bool
	top      string // git toplevel, symlinks resolved
	repo     string // origin, as an https URL; "" when it is not to be named
	why      string // why repo is empty, for -v
	isPublic func(repo string) (bool, error)
}

func newSourceResolver(e *Env, disabled bool) *sourceResolver {
	return &sourceResolver{e: e, disabled: disabled, isPublic: repoIsPublic}
}

func (r *sourceResolver) init() {
	if r.once {
		return
	}
	r.once = true
	top, err := git(r.e.Root, "rev-parse", "--show-toplevel")
	if err != nil {
		r.why = "not a git checkout"
		return
	}
	r.top = strings.TrimSpace(top)
	if t, err := filepath.EvalSymlinks(r.top); err == nil {
		r.top = t
	}
	remote, err := git(r.e.Root, "remote", "get-url", "origin")
	if err != nil {
		r.why = "no origin remote"
		return
	}
	repo, err := normalizeRemote(remote)
	if err != nil {
		r.why = err.Error()
		return
	}
	public, err := r.isPublic(repo)
	switch {
	case err != nil:
		r.why = fmt.Sprintf("could not tell whether %s is public (%v)", repo, err)
	case !public:
		r.why = repo + " is not public"
	default:
		r.repo = repo
	}
}

// section returns the [source] table to upload for the package in dir, a
// workspace-relative directory, given the gnomod.toml body on disk. The zero
// value means: upload no section.
func (r *sourceResolver) section(dir, gnomod string) (sourceSection, error) {
	if r.disabled {
		return sourceSection{}, nil
	}
	_, declared, found, err := splitSource(gnomod)
	if err != nil {
		return sourceSection{}, fmt.Errorf("%s/gnomod.toml: %w", dir, err)
	}
	r.init()
	s := sourceSection{Repository: r.repo}
	if found && declared.Repository != "" {
		s = declared
	} else if s.Repository == "" {
		r.e.tracef("source   %s: no [source], %s\n", dir, r.why)
		return sourceSection{}, nil
	}
	if r.top == "" {
		// Declared, but not in a checkout: upload what the author wrote.
		if err := s.validate(); err != nil {
			return sourceSection{}, fmt.Errorf("%s/gnomod.toml: %w", dir, err)
		}
		return s, nil
	}
	abs := filepath.Join(r.e.Root, filepath.FromSlash(dir))
	if a, err := filepath.EvalSymlinks(abs); err == nil {
		abs = a
	}
	if s.Path == "" {
		rel, err := filepath.Rel(r.top, abs)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			s.Path = filepath.ToSlash(rel)
		}
	}
	s.Revision = ""
	dirty, err := gitDirty(r.e.Root, dir)
	switch {
	case err != nil:
		r.e.tracef("source   %s: no revision, %v\n", dir, err)
	case len(dirty) > 0:
		r.e.tracef("source   %s: no revision, %d uncommitted change(s)\n", dir, len(dirty))
	default:
		if head, err := gitHead(r.e.Root); err == nil {
			s.Revision = head
		}
	}
	if err := s.validate(); err != nil {
		return sourceSection{}, fmt.Errorf("%s/gnomod.toml: %w", dir, err)
	}
	rev := s.Revision
	if len(rev) > 9 {
		rev = rev[:9]
	}
	r.e.tracef("source   %s: %s/%s@%s\n", dir, s.Repository, s.Path, rev)
	return s, nil
}

var scpRemote = regexp.MustCompile(`^(?:[\w.-]+@)?([\w.-]+):([^/].*)$`)

// normalizeRemote turns a git remote into the https URL of the repository.
// git@github.com:o/r.git, ssh://git@github.com/o/r and
// https://token@github.com/o/r.git all become https://github.com/o/r, and
// credentials are always dropped, since the result goes on chain. Mirrors
// gitsource.NormalizeRemote (gnolang/gno#6282, read 2026-10-06).
func normalizeRemote(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	var host, p string
	if m := scpRemote.FindStringSubmatch(remote); m != nil && !strings.Contains(remote, "://") {
		host, p = m[1], m[2]
	} else {
		u, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("origin %q is not a URL", remote)
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return "", fmt.Errorf("origin %q is not a hosted repository", remote)
		}
		host, p = u.Hostname(), u.Path
	}
	p = strings.Trim(strings.TrimSuffix(strings.Trim(p, "/"), ".git"), "/")
	if host == "" || p == "" {
		return "", fmt.Errorf("origin %q names no repository", remote)
	}
	return "https://" + host + "/" + p, nil
}

// repoIsPublic is the question the resolver asks, a variable so the test suite
// can cut it off from the network (keybase_test.go's TestMain).
var repoIsPublic = githubRepoIsPublic

// githubRepoIsPublic asks GitHub, without credentials, whether repo is public.
//
// Without credentials on purpose: a token that can read a private repository
// gets a 200 for it, and "I could read it" is exactly the wrong test for "may
// this name go on chain". Only github.com is asked; for any other host the
// answer is no, and the author can still declare [source] by hand.
func githubRepoIsPublic(repo string) (bool, error) {
	u, err := url.Parse(repo)
	if err != nil || u.Host != "github.com" {
		return false, nil
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return false, nil
	}
	base := os.Getenv("GITHUB_API_URL")
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequest("GET", strings.TrimSuffix(base, "/")+"/repos/"+parts[0]+"/"+parts[1], nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// What GitHub answers a stranger about a private repository.
		return false, nil
	default:
		return false, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var out struct {
		Private bool `json:"private"`
	}
	if err := json.NewDecoder(newLimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return false, err
	}
	return !out.Private, nil
}
