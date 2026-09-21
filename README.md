# gnopm

[![CI](https://github.com/moul/gnopm/actions/workflows/ci.yml/badge.svg)](https://github.com/moul/gnopm/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/moul.io/gnopm.svg)](https://pkg.go.dev/moul.io/gnopm)
[![Go Report Card](https://goreportcard.com/badge/moul.io/gnopm)](https://goreportcard.com/report/moul.io/gnopm)
[![License](https://img.shields.io/badge/license-Apache--2.0%20%2F%20MIT-%2397ca00.svg)](./COPYRIGHT)
[![Site](https://img.shields.io/badge/docs-moul.github.io%2Fgnopm-blue)](https://moul.github.io/gnopm/)

**A package manager for [gno](https://gno.land) workspaces.** The version of a
package lives in its `gnomod.toml`, not in its directory name.

```sh
go install moul.io/gnopm@latest
```

## Why

A gno package path ends in its version, so the obvious layout gives each
version its own directory, and bumping means copying that directory.

**git cannot pair a copy.** The diff becomes added files with no content diff,
so the compatibility change, the one thing that most needs reviewing, is the
one thing you cannot see. `git log --follow`, `blame` and `bisect` stop there
too. One real port of 25 realms landed as **+11,103 / -0 across 112 files**.

gnopm removes the copy: the toolchain already reads the version from the module
line, so the directory need not repeat it.

```
p/alice/md/gnomod.toml     module = "gno.land/p/alice/md/v1"
p/alice/md/md.gno          edited in place
```

A bump is one line, and then the real diff:

<p align="center">
  <img src="docs/img/bump.svg" alt="gnopm bump changes one line in gnomod.toml, and git records a real diff" width="760">
</p>

Superseded versions are pinned in `gnomod.lock` to the commit that still holds
them and rebuilt into a gitignored `.gnopm/` on demand, so anything importing
`.../md/v0` keeps resolving.

## Use

```sh
gnopm status      # what resolves, and whether anything is out of date
gnopm sync        # make the state good
gnopm bump md     # promote a package, in place; then edit the files
gnopm ls          # every module and where its source is
gnopm verify      # prove every pinned version still reproduces (for CI)
```

<p align="center">
  <img src="docs/img/status.svg" alt="gnopm status" width="560">
</p>

`status` to ask, `sync` to fix. `<package>` is a directory, a module path, or
any unambiguous part of one; with no argument `bump` uses the package you are
standing in. Data goes to stdout and nothing else does, so it pipes:

```sh
gnopm ls -q | xargs -n1 gno lint
gnopm status -json | jq -e .ok
```

Migrating a repository off versioned directories is one command, and it shows
the plan first:

```sh
gnopm deversion -n
gnopm deversion
```

Full command reference: `gnopm help <command>`, or
**[moul.github.io/gnopm](https://moul.github.io/gnopm/)**.

## `gnomod.lock`

```toml
lock = 1

[[module]]
module = "gno.land/p/alice/md/v0"
source = { commit = "d387abaa3a81...", dir = "p/alice/md/v0" }
hash = "h1:3f9c..."

[[module]]
module = "gno.land/p/alice/md/v1"
source = { dir = "p/alice/md" }
```

One entry per module path; `hash` is `h1:`, as in `go.sum`. It is **source, not
a generated artifact**: a change that bumps a version carries the pin keeping
the old one resolvable. It changes only on add, remove or bump.

Named after the manifest rather than the tool, at the workspace root only, as
every ecosystem does it.

**Pins go to a commit already on the default branch.** Most repositories
squash-merge, so a pin to a branch commit stops resolving once the change
lands. `verify` catches that early; CI needs `fetch-depth: 0`, because a
shallow clone has none of the pinned commits.

## As a library

| import | what |
|---|---|
| [`moul.io/gnopm/pkg/gnomodlock`](./pkg/gnomodlock) | the `gnomod.lock` format, with no dependency on the CLI |
| [`moul.io/gnopm/pkg/gnopm`](./pkg/gnopm) | the operations: `Sync`, `Bump`, `Verify`, `Tidy`, `Deversion` |

No third-party dependencies. The root is the command and a high-level
integration test, nothing else.

## More

- **[moul/gnopm-demo](https://github.com/moul/gnopm-demo)** is a generated
  worked example. Read it as a history: `git log --reverse --stat`.
- [`scripts/demo.sh`](./scripts/demo.sh) builds it and is the integration test.
  A demo that is not executed rots; one executed as a test cannot. The terminal
  images above are captured the same way.
- [Roadmap and design notes](https://github.com/moul/gnopm/issues/2) ·
  [Migrating a repository](https://github.com/moul/gnopm/issues/3) ·
  [CONTRIBUTING](./CONTRIBUTING.md)

## License

© 2026 [Manfred Touron](https://moul.io). Apache-2.0 or MIT, at your option.
See [`COPYRIGHT`](./COPYRIGHT).
