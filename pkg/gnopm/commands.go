package gnopm

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// Status answers "what is the state of this workspace", in three lines.
//
// Read-only on purpose. A status command that silently repaired things would
// make it impossible to ask the question, and the answer always names the
// command that fixes what it found.
func Status(e *Env) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	pkgs, err := scanPackages(e.Root)
	if err != nil {
		return err
	}
	pinned := materializedEntries(lock)

	lockOK, lockWhy := lockState(lock, pkgs)
	asmOK, asmWhy := assemblyState(e.Root, lock, pinned)

	rec := StatusRecord{
		Modules:  len(lock.Modules),
		Tree:     len(pkgs),
		Pinned:   len(pinned),
		OK:       lockOK && asmOK,
		Lock:     lockWhy,
		Assembly: asmWhy,
	}
	if e.JSON {
		return e.writeJSON(rec)
	}
	if e.Format != "" {
		return e.emit(rec)
	}

	e.printf("%d modules   %d in tree   %d pinned to history\n", len(lock.Modules), len(pkgs), len(pinned))
	if len(lock.Modules) == 0 {
		e.printf("\nno %s yet. run `gnopm sync`\n", lockFile)
		return nil
	}
	e.printf("lock         %s\n", lockWhy)
	e.printf("assembly     %s\n", asmWhy)
	if !lockOK || !asmOK {
		e.printf("\nrun `gnopm sync`\n")
	}
	return nil
}

// lockState reports whether the lock still describes the tree, and why not.
func lockState(lock *Lock, pkgs []Package) (bool, string) {
	if err := consistent(lock, pkgs); err != nil {
		return false, "stale: " + err.Error()
	}
	return true, "up to date"
}

// assemblyState reports whether .gnopm/ holds exactly what the lock pins.
func assemblyState(root string, lock *Lock, pinned []LockEntry) (bool, string) {
	if len(pinned) == 0 {
		return true, "nothing to materialize"
	}
	stamp, err := os.ReadFile(filepath.Join(root, assemblyDir, stampFile))
	if err != nil {
		return false, fmt.Sprintf("%d version(s) not materialized", len(pinned))
	}
	if strings.TrimSpace(string(stamp)) != hashString(lock.String()) {
		return false, "out of date"
	}
	return true, fmt.Sprintf("up to date (%d materialized)", len(pinned))
}

// cmdLs lists modules. Data on stdout, nothing else, so it pipes.
func cmdLs(e *Env, fs *flag.FlagSet, args []string) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	pattern := ""
	if len(args) > 0 {
		pattern = args[0]
	}
	onlyPinned, onlyTree := flagBool(fs, "pinned"), flagBool(fs, "tree")

	var rows []LockEntry
	for _, m := range sortedModules(lock) {
		if pattern != "" && !strings.Contains(m.Module, pattern) && !strings.Contains(m.Source.Dir, pattern) {
			continue
		}
		if onlyPinned && m.Source.InTree() {
			continue
		}
		if onlyTree && !m.Source.InTree() {
			continue
		}
		rows = append(rows, m)
	}

	recs := make([]LsRecord, 0, len(rows))
	for _, m := range rows {
		recs = append(recs, LsRecord{
			Module: m.Module,
			Source: m.Source.Variant(),
			Dir:    m.Source.Dir,
			Commit: m.Source.Commit,
			Hash:   m.Hash,
		})
	}
	if e.JSON {
		return e.writeJSON(recs)
	}
	if e.Format != "" {
		vals := make([]any, len(recs))
		for i := range recs {
			vals[i] = recs[i]
		}
		return e.emit(vals...)
	}
	if e.Quiet {
		for _, m := range rows {
			e.printf("%s\n", m.Module)
		}
		return nil
	}
	w := tabwriter.NewWriter(e.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODULE\tSOURCE\tLOCATION")
	for _, m := range rows {
		loc := m.Source.Dir
		kind := "tree"
		if !m.Source.InTree() {
			kind = "history"
			loc = short(m.Source.Commit) + ":" + m.Source.Dir
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", m.Module, kind, loc)
	}
	return w.Flush()
}

// Sync is the lazy one: work out what is out of date and fix it.
//
// Everything else is a special case of this, which is why it is the command
// a Makefile prerequisite or a shell hook should call. Quiet when there is
// nothing to do, so running it too often costs nothing.
func Sync(e *Env) error {
	if err := Relock(e); err != nil {
		return err
	}
	return Install(e)
}

func Relock(e *Env) error {
	pkgs, err := scanPackages(e.Root)
	if err != nil {
		return err
	}
	old, err := readLock(e.Root)
	if err != nil {
		return err
	}
	next, err := buildLock(old, pkgs)
	if err != nil {
		return err
	}
	// Compare against the bytes on disk, not against the re-serialized old
	// lock. Those differ whenever the generated header's wording changes, and
	// comparing structs would leave a stale header on disk forever, telling
	// people to run commands that no longer exist. verify tolerates that;
	// sync is what quietly repairs it.
	raw, _ := os.ReadFile(filepath.Join(e.Root, lockFile))
	if string(raw) == next.String() {
		return nil
	}
	changed := len(raw) == 0 || !sameLockData(string(raw), next.String())
	if err := writeLock(e.Root, next); err != nil {
		return err
	}
	if changed {
		e.logf("%s updated: %d modules (%d in tree, %d pinned to history)\n",
			lockFile, len(next.Modules), len(pkgs), len(next.Modules)-len(pkgs))
	}
	return nil
}

func cmdBump(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) == 0 {
		// No argument: if you are standing in a package, that is the one you
		// meant. Asking would be asking a question gnopm can answer.
		pkg, err := packageAtCwd(e.Root)
		if err != nil {
			return err
		}
		args = []string{pkg}
		e.logf("bumping %s (from the current directory)\n", pkg)
	}
	if len(args) > 1 {
		return fmt.Errorf("bump takes one package, got %d", len(args))
	}
	// Lazily bring the lock in line first: bumping against a stale lock would
	// record a pin next to entries that do not describe the tree.
	if err := Relock(e); err != nil {
		return err
	}
	if err := Bump(e, args[0], BumpOptions{
		To:          flagInt(fs, "to"),
		Force:       flagBool(fs, "force"),
		IfPublished: flagBool(fs, "if-published"),
		RPC:         flagString(fs, "rpc"),
		ChainID:     flagString(fs, "chainid"),
	}); err != nil {
		return err
	}
	// And materialize the version just pinned, so whatever still imports it
	// resolves without a second command.
	return Install(e)
}

func cmdDeversion(e *Env, fs *flag.FlagSet, args []string) error {
	if err := Deversion(e, flagBool(fs, "n")); err != nil {
		return err
	}
	if flagBool(fs, "n") {
		return nil
	}
	return Install(e)
}

func (e *Env) writeJSON(v any) error {
	enc := json.NewEncoder(e.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// cmdVersion reports what this binary is, the way every other tool does.
// Its absence was a paper cut: `gnopm version` suggested `gnopm verify`.
func cmdVersion(e *Env) error {
	v, rev, dirty := buildVersion()
	rec := VersionRecord{Version: v, Revision: rev, Dirty: dirty}
	if e.JSON {
		return e.writeJSON(rec)
	}
	if e.Format != "" {
		return e.emit(rec)
	}
	line := "gnopm " + v
	if rev != "" {
		line += " (" + rev
		if dirty {
			line += ", dirty"
		}
		line += ")"
	}
	e.printf("%s\n", line)
	return nil
}

// cmdEnv prints what gnopm worked out about this machine and this workspace.
//
// Everything here is detected rather than configured, so this is also the
// answer to "why did it do that": if a command surprises you, env shows the
// inputs it surprised you with.
func cmdEnv(e *Env) error {
	lock := filepath.Join(e.Root, lockFile)
	if _, err := os.Stat(lock); err != nil {
		lock = "(none yet)"
	}
	upstream := upstreamRef(e.Root, "")
	if upstream == "" {
		upstream = "(none detected)"
	}
	cache := e.cacheDir()
	if cache == "" {
		cache = "(off)"
	}
	rec := EnvRecord{
		Root:     e.Root,
		Lock:     lock,
		Assembly: filepath.Join(e.Root, assemblyDir),
		Upstream: upstream,
		Cache:    cache,
		GnoHome:  gnoHome(),
	}
	if e.JSON {
		return e.writeJSON(rec)
	}
	if e.Format != "" {
		return e.emit(rec)
	}
	for _, kv := range [][2]string{
		{"GNOPM_ROOT", rec.Root},
		{"GNOPM_LOCK", rec.Lock},
		{"GNOPM_ASSEMBLY", rec.Assembly},
		{"GNOPM_UPSTREAM", rec.Upstream},
		{"GNOPM_CACHE", rec.Cache},
		{"GNOHOME", rec.GnoHome},
	} {
		e.printf("%s=%q\n", kv[0], kv[1])
	}
	return nil
}

// gnoHome mirrors the gno toolchain's own home, because gnopm should cache
// beside it rather than inventing a second directory nobody knows about.
func gnoHome() string {
	if h := os.Getenv("GNOHOME"); h != "" {
		return h
	}
	if c, err := os.UserConfigDir(); err == nil {
		return filepath.Join(c, "gno")
	}
	return ""
}

// packageAtCwd finds the package the working directory is inside.
//
// Nearest enclosing directory with a gnomod.toml, so it works from a package's
// own subdirectory (filetests/, say) too.
func packageAtCwd(root string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	wd, err = filepath.Abs(wd)
	if err != nil {
		return "", err
	}
	for d := wd; strings.HasPrefix(d, root); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "gnomod.toml")); err == nil {
			rel, err := filepath.Rel(root, d)
			if err != nil {
				return "", err
			}
			return filepath.ToSlash(rel), nil
		}
		if d == root {
			break
		}
	}
	return "", fmt.Errorf("no package here, and none given. cd into one, or name it: `gnopm bump <package>`\n" +
		"  `gnopm ls -q` lists them")
}

func cmdUnbump(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) == 0 {
		pkg, err := packageAtCwd(e.Root)
		if err != nil {
			return err
		}
		args = []string{pkg}
		e.logf("unbumping %s (from the current directory)\n", pkg)
	}
	if len(args) > 1 {
		return fmt.Errorf("unbump takes one package, got %d", len(args))
	}
	if err := Relock(e); err != nil {
		return err
	}
	if err := Unbump(e, args[0], UnbumpOptions{
		Force:   flagBool(fs, "force"),
		RPC:     flagString(fs, "rpc"),
		ChainID: flagString(fs, "chainid"),
	}); err != nil {
		return err
	}
	return Install(e)
}
