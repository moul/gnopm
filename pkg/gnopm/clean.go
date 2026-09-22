package gnopm

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// CleanOptions is what clean was asked to remove.
type CleanOptions struct {
	// Root is the workspace whose assembly goes. "" cleans no assembly, which
	// is what `gnopm clean -cache` outside a workspace does.
	Root string
	// Cache also removes the shared chain cache, the way `go clean -cache`
	// does. Off by default: the assembly belongs to this workspace, the cache
	// is shared with every other one on the machine.
	Cache bool
	// DryRun prints what would go and removes nothing.
	DryRun bool
}

// Clean removes what gnopm generated and can rebuild.
//
// It exists because the alternative is a line of `rm -rf` in every adopting
// repository's Makefile, and that is the wrong owner: gnopm created the
// directory, gnopm knows its name, and a hand-written path in somebody else's
// Makefile is a hard-coded copy of a constant that may move.
//
// Nothing here is source. The assembly is rebuilt byte for byte from
// gnomod.lock by `gnopm sync`, and the cache is an optimisation whose absence
// costs a chain read. Removing either can lose nothing, which is why this
// command can be blunt about it.
func Clean(e *Env, opts CleanOptions) error {
	type target struct {
		what, path string
	}
	var targets []target
	if opts.Root != "" {
		targets = append(targets, target{"assembly", filepath.Join(opts.Root, assemblyDir)})
	}
	if opts.Cache {
		dir := cacheDir()
		if dir == "" {
			e.logf("cache        nothing to remove: %s is off, or there is no home directory\n", cacheEnv)
		} else if err := safeToRemove(dir); err != nil {
			return fmt.Errorf("refusing to remove the cache at %s: %w", dir, err)
		} else {
			targets = append(targets, target{"cache", dir})
		}
	}

	removed := 0
	for _, t := range targets {
		files, bytes, err := dirSize(t.path)
		if err != nil {
			return err
		}
		if files == 0 {
			if opts.DryRun {
				e.logf("%-12s nothing to remove (%s)\n", t.what, t.path)
			}
			continue
		}
		verb := "removed"
		if opts.DryRun {
			verb = "would remove"
		}
		e.logf("%-12s %s %s (%d file%s, %s)\n", t.what, verb, t.path, files, plural(files, "", "s"), humanBytes(bytes))
		if opts.DryRun {
			continue
		}
		if err := os.RemoveAll(t.path); err != nil {
			return err
		}
		removed++
	}
	if removed > 0 && opts.Root != "" {
		// Say what undoes it. A command that deletes and then says nothing
		// leaves the reader to work out whether they just broke the workspace.
		e.logf("             `gnopm sync` rebuilds the assembly from %s\n", lockFile)
	}
	return nil
}

func cmdClean(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("clean takes no arguments, got %q", args[0])
	}
	cache := flagBool(fs, "cache")
	opts := CleanOptions{Cache: cache, DryRun: flagBool(fs, "n")}

	// Detect, do not ask. Cleaning the shared cache is a machine-wide job and
	// has nothing to do with where you are standing, so being outside a
	// workspace is not an error when that is all that was asked for.
	root, err := FindRoot(flagString(fs, "C"))
	switch {
	case err == nil:
		opts.Root = root
	case cache:
		e.logf("assembly     skipped: no %s here, so there is no workspace to clean\n", workspaceMarker)
	default:
		return err
	}
	return Clean(e, opts)
}

// dirSize reports how many files a directory holds and how many bytes they are.
// A missing directory is zero of both, not an error: clean is idempotent.
func dirSize(dir string) (files int, bytes int64, err error) {
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		bytes += info.Size()
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		return 0, 0, nil
	}
	return files, bytes, err
}

// safeToRemove refuses the paths a typo in GNOPM_CACHE could aim at.
//
// The cache directory comes from an environment variable, so `GNOPM_CACHE=$HOME
// gnopm clean -cache` is one shell expansion away from being a very bad
// afternoon. Nothing below is a real use of the flag, so refusing costs
// nothing.
func safeToRemove(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) || abs == filepath.VolumeName(abs)+string(filepath.Separator) {
		return fmt.Errorf("that is the filesystem root")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if abs == filepath.Clean(home) {
			return fmt.Errorf("that is your home directory")
		}
	}
	if st, err := os.Stat(abs); err == nil && !st.IsDir() {
		return fmt.Errorf("that is a file, not a directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// humanBytes renders a byte count the way a human reads one.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
