// Command gnopm is a package manager for gno workspaces.
//
// It keeps a package's version in its gnomod.toml instead of in its directory
// name, records where every version's source lives in a gnomod.lock, and
// rebuilds the versions that are no longer in the working tree so that pinned
// imports still resolve.
//
// Install it with:
//
//	go install moul.io/gnopm@latest
//
// Everything interesting lives in the packages, so that other programs can use
// it without shelling out:
//
//	moul.io/gnopm/pkg/gnopm        the operations
//	moul.io/gnopm/pkg/gnomodlock   the gnomod.lock format, on its own
//
// This file is only the entry point.
package main

import (
	"fmt"
	"os"

	"moul.io/gnopm/pkg/gnopm"
)

func main() {
	if err := gnopm.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gnopm: "+err.Error())
		os.Exit(1)
	}
}
