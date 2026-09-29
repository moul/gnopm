package gnopm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpstreamIsDetectedNotDemanded: CI used to have to pass
// -upstream origin/main. Every required flag is friction paid forever, and
// this one was avoidable.
func TestUpstreamIsDetectedNotDemanded(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/a", "gno.land/p/moul/a/v0", "package a\n")
	commit(t, root, "seed")
	// A remote-tracking ref is all the detection needs.
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	if got := upstreamRef(root, ""); got != "origin/main" {
		t.Fatalf("detected %q, want origin/main", got)
	}
	if got := upstreamRef(root, "refs/heads/whatever"); got != "refs/heads/whatever" {
		t.Fatalf("an explicit value must win, got %q", got)
	}
	// A pull request's base branch wins over the default branch.
	gitCmd(t, root, "update-ref", "refs/remotes/origin/release", "HEAD")
	t.Setenv("GITHUB_BASE_REF", "release")
	if got := upstreamRef(root, ""); got != "origin/release" {
		t.Fatalf("GITHUB_BASE_REF ignored, got %q", got)
	}
}

// TestNoRemoteIsNotAnError: a repository with no remote is a legitimate place
// to work, and refusing to operate there would be worse than not checking.
func TestNoRemoteIsNotAnError(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/a", "gno.land/p/moul/a/v0", "package a\n")
	commit(t, root, "seed")
	if got := upstreamRef(root, ""); got != "" {
		t.Fatalf("invented an upstream out of nothing: %q", got)
	}
	mustRun(t, root, "sync")
	if err := Verify(testEnv(root, &bytes.Buffer{})); err != nil {
		t.Fatalf("verify must work without a remote: %v", err)
	}
}

// TestTidyNeedsBothConditions is the whole safety argument for tidy: unused is
// not permission to forget, only unused AND never-shipped is.
func TestTidyNeedsBothConditions(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	// A bump that lands entirely on this branch: v0 never shipped.
	gitCmd(t, root, "checkout", "-q", "-b", "feature")
	if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump")

	// v0's pin IS upstream here (bump pinned it to origin/main), so tidy must
	// keep it: it shipped.
	var out bytes.Buffer
	if err := Tidy(testEnv(root, &out), TidyOptions{Offline: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to drop") {
		t.Fatalf("tidy dropped a version that shipped: %s", out.String())
	}

	// Now the unshipped case: edit then bump, so the pin is branch-only.
	write(t, filepath.Join(root, "p/moul/md/md.gno"), "package md // v1 body\n")
	commit(t, root, "edit")
	if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump again")
	out.Reset()
	if err := Tidy(testEnv(root, &out), TidyOptions{Offline: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "gno.land/p/moul/md/v1") {
		t.Fatalf("tidy kept a version that never shipped and nobody imports: %s", out.String())
	}
	// And the stranded pin is gone, so the upstream guard is satisfied again.
	if err := VerifyWith(testEnv(root, &bytes.Buffer{}), "origin/main"); err != nil {
		t.Fatalf("after tidy the upstream check should pass: %v", err)
	}
}

// TestTidyKeepsAnImportedVersion: still imported means still needed, whatever
// its provenance.
func TestTidyKeepsAnImportedVersion(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	addPkg(t, root, "p/moul/user", "gno.land/p/moul/user/v0",
		"package user\n\nimport \"gno.land/p/moul/md/v0\"\n\nfunc U() { _ = md.X }\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(root, "p/moul/md/md.gno"), "package md // edited\n")
	commit(t, root, "edit md")
	if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump md")

	var out bytes.Buffer
	if err := Tidy(testEnv(root, &out), TidyOptions{Offline: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to drop") {
		t.Fatalf("tidy dropped a version that p/moul/user imports: %s", out.String())
	}
}

// TestBumpFindsThePackageYouAreStandingIn.
func TestBumpFindsThePackageYouAreStandingIn(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	write(t, filepath.Join(root, "p/moul/md/filetests/z_x_filetest.gno"), "package main\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")

	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	// From a subdirectory of the package, not even the package root.
	if err := os.Chdir(filepath.Join(root, "p/moul/md/filetests")); err != nil {
		t.Fatal(err)
	}
	got, err := packageAtCwd(root, "bump")
	if err != nil {
		t.Fatal(err)
	}
	if got != "p/moul/md" {
		t.Fatalf("found %q, want p/moul/md", got)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	if _, err := packageAtCwd(root, "bump"); err == nil {
		t.Fatal("the workspace root is not a package; it must say so")
	}
}

// TestPackageAtCwdFollowsASymlinkedWorkspace pins moul/gnopm#75: `gnopm bump`
// and `gnopm unbump` with no argument answered "no package here" from inside a
// package, whenever the workspace path went through a symlink.
//
// Two things have to line up. FindRoot returns the spelling it was given, with
// symlinks intact. os.Getwd returns $PWD when $PWD still names the working
// directory, and the kernel's resolved path otherwise. The old condition
// compared the two with strings.HasPrefix, and /tmp/w and /private/tmp/w share
// no prefix at all.
//
// So an interactive shell hid it, because a shell keeps $PWD in the symlinked
// spelling and the two then agree. A process that chdir'd hands its children
// no usable $PWD, and that is where it bit: verified at the CLI with
// `env -u PWD gnopm -C /tmp/w unbump` from inside a package, which failed
// before this change and works after.
//
// os.Chdir in a test is exactly that shape, which is why
// TestBumpFindsThePackageYouAreStandingIn failed on every macOS checkout and
// passed in CI, where /tmp on a Linux runner is a real directory and the two
// spellings coincide. The symlink here is made explicitly rather than
// inherited from the platform, so this fails on the old code on Linux too.
func TestPackageAtCwdFollowsASymlinkedWorkspace(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")

	link := filepath.Join(t.TempDir(), "via-a-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(filepath.Join(link, "p/moul/md")); err != nil {
		t.Fatal(err)
	}
	// The root as the caller spells it, which is the unresolved one.
	got, err := packageAtCwd(link, "bump")
	if err != nil {
		t.Fatalf("inside the package through a symlink: %v", err)
	}
	if got != "p/moul/md" {
		t.Fatalf("found %q, want p/moul/md", got)
	}
}

// TestWithinComparesPathsNotStrings: the old condition was
// strings.HasPrefix(d, root), which puts /a/workspace-old inside /a/workspace
// and would walk a sibling tree looking for a package.
//
// Unreachable from packageAtCwd's own tests, because reaching it needs a
// working directory outside the root with a name that starts like it, which is
// a shape no fixture builds by accident.
func TestWithinComparesPathsNotStrings(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	for _, tc := range []struct {
		name string
		d    string
		want bool
	}{
		{"the root itself", root, true},
		{"a package in it", filepath.Join(root, "p/moul/md"), true},
		{"its parent", base, false},
		{"a sibling that starts the same", root + "-old", false},
		{"a sibling entirely", filepath.Join(base, "elsewhere"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := within(root, tc.d); got != tc.want {
				t.Errorf("within(%q, %q) = %v, want %v", root, tc.d, got, tc.want)
			}
		})
	}
}
