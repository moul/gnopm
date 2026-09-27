package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDemoScript runs scripts/demo.sh into a temporary directory.
//
// The script builds a whole repository from nothing and asserts at every step,
// so this is the integration test: it exercises the real binary against real
// git, and it covers the things unit tests structurally cannot, like whether
// git records the migration as renames or whether a version with no directory
// left still materializes byte-for-byte.
//
// It also happens to produce the published demo repository, which is the point
// of writing it this way: a demo that is not executed rots, and a demo that is
// executed as a test cannot.
func TestDemoScript(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and a git repository")
	}
	for _, bin := range []string{"bash", "git", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	script, err := filepath.Abs(filepath.Join("scripts", "demo.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("demo script missing: %v", err)
	}

	// Build the binary once and hand it to the script, so the test exercises
	// this working tree rather than whatever `gnopm` happens to be on PATH.
	bin := filepath.Join(t.TempDir(), "gnopm")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building gnopm: %v\n%s", err, out)
	}

	repo := filepath.Join(t.TempDir(), "gnopm-demo")
	cmd := exec.Command("bash", script, repo)
	cmd.Env = append(os.Environ(),
		"GNOPM="+bin,
		// A hermetic git identity: the script commits, and a machine with no
		// user.name configured would otherwise fail here and nowhere else.
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("demo.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "all assertions passed") {
		t.Fatalf("demo.sh did not reach the end:\n%s", out)
	}

	// Spot-check the artefact itself, not just the script's own assertions.
	for _, want := range []string{
		"README.md",
		"gnomod.lock",
		"p/demo/strs/gnomod.toml", // de-versioned directory
		".gnopm/gno.land/p/demo/strs/v0/strs.gno",     // superseded, rebuilt from history
		"vendor/gno.land/p/nt/tinyavl/v0/gnomod.toml", // external, untouched
	} {
		if _, err := os.Stat(filepath.Join(repo, want)); err != nil {
			t.Errorf("demo repo is missing %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "p/demo/strs/v0")); !os.IsNotExist(err) {
		t.Error("p/demo/strs/v0 should have no directory after the migration")
	}
}

// TestREADMENamesRealCommands walks every `gnopm <word>` in the README and
// checks the command exists.
//
// The README is the most-read artifact in the repository and the one most
// likely to promise something that was planned and never landed, or that was
// renamed afterwards. This is the same guard
// TestForeignAdviceNamesRealCommands applies to the cross-ecosystem table, and
// it caught the same class of thing there twice.
//
// It shells out to the built binary rather than importing the command table,
// because the root package deliberately holds no logic, and because `gnopm help
// <command>` failing is the user-visible symptom this is about.
func TestREADMENamesRealCommands(t *testing.T) {
	// Deliberate mentions of commands that do not exist. Each one is a
	// sentence explaining an absence, so the words are load-bearing and must
	// not be "fixed" by adding the command.
	absent := map[string]bool{
		"update": true, // "there is no `gnopm update`", and there should not be
		"help":   true, // real, but `gnopm help help` is not a topic
	}

	bin := filepath.Join(t.TempDir(), "gnopm")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building gnopm: %v\n%s", err, out)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile("`gnopm ([a-z-]+)").FindAllStringSubmatch(string(readme), -1) {
		name := m[1]
		if seen[name] || absent[name] {
			continue
		}
		seen[name] = true
		if err := exec.Command(bin, "help", name).Run(); err != nil {
			t.Errorf("the README says `gnopm %s`, which is not a command", name)
		}
	}
	if len(seen) < 10 {
		t.Errorf("only found %d commands in the README, so the pattern is probably wrong", len(seen))
	}
}
