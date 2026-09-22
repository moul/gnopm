package gnopm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClean asserts the claim that makes the command safe to be blunt about:
// what it removes, sync puts back byte for byte.
//
// Anything less and clean is a command nobody dares run, which is how the
// rm -rf line ended up in every adopting repository's Makefile in the first
// place.
func TestClean(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	addPkg(t, root, "r/b", "gno.land/r/b/v0", "package b\n\nimport \"gno.land/p/a/v0\"\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "p/a")

	asm := filepath.Join(root, assemblyDir)
	before := snapshotDir(t, asm)
	if len(before) == 0 {
		t.Fatal("nothing materialized, so this test proves nothing")
	}

	// -n has to leave it alone. Nobody should first meet a delete command
	// without a way to look before it writes.
	out := mustRun(t, root, "clean", "-n")
	if !strings.Contains(out, "would remove") {
		t.Fatalf("clean -n did not say what it would remove:\n%s", out)
	}
	if got := snapshotDir(t, asm); len(got) != len(before) {
		t.Fatalf("clean -n removed %d file(s)", len(before)-len(got))
	}

	// The real thing, and nothing on stdout: every line here is a diagnostic.
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"clean", "-C", root}, &stdout, &stderr); err != nil {
		t.Fatalf("clean: %v\n%s", err, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("clean wrote to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gnopm sync") {
		t.Fatalf("clean did not name what rebuilds it:\n%s", stderr.String())
	}
	if _, err := os.Stat(asm); !os.IsNotExist(err) {
		t.Fatalf("the assembly survived clean: %v", err)
	}

	// Idempotent and silent: a second clean has nothing to say.
	var again, againErr bytes.Buffer
	if err := Run([]string{"clean", "-C", root}, &again, &againErr); err != nil {
		t.Fatalf("second clean: %v", err)
	}
	if again.String() != "" || againErr.String() != "" {
		t.Fatalf("a no-op clean was not silent: %q / %q", again.String(), againErr.String())
	}

	mustRun(t, root, "sync")
	after := snapshotDir(t, asm)
	if len(after) != len(before) {
		t.Fatalf("sync rebuilt %d file(s), had %d", len(after), len(before))
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("%s did not come back byte for byte", name)
		}
	}
}

// TestCleanCache covers the flag that can reach outside the workspace, and the
// guard that keeps a shell expansion from making it catastrophic.
func TestCleanCache(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	cache := t.TempDir()
	write(t, filepath.Join(cache, "live", "gnoland-1"), "gno.land/p/a/v0\n")
	t.Setenv(cacheEnv, cache)
	if out := mustRun(t, root, "clean", "-cache"); !strings.Contains(out, cache) {
		t.Fatalf("clean -cache did not name the cache:\n%s", out)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("the cache survived clean -cache: %v", err)
	}

	// The cache directory comes from an environment variable, so the one
	// mistake that matters is it expanding to something that is not a cache.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory to guard")
	}
	t.Setenv(cacheEnv, home)
	var out, errw bytes.Buffer
	if err := Run([]string{"clean", "-C", root, "-cache"}, &out, &errw); err == nil {
		t.Fatal("clean -cache agreed to remove the home directory")
	} else if !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("the refusal does not say why: %v", err)
	}

	// Off is not an error, and not a reason to remove anything.
	t.Setenv(cacheEnv, "off")
	if out := mustRun(t, root, "clean", "-cache", "-n"); !strings.Contains(out, "is off") {
		t.Fatalf("a disabled cache was not reported:\n%s", out)
	}
}

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = read(t, p)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}
