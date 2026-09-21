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

**git cannot pair a copy.** The review diff of a version bump becomes a set of
added files with no content diff at all, which is backwards: a bump is by
definition the compatibility change that most needs reviewing, and it is the
one change you cannot see. `git log --follow`, `blame` and `bisect` stop there
too. One real port of 25 realms landed as **+11,103 / -0 across 112 files**,
with every behaviour change invisible.

gnopm removes the copy. The toolchain already reads the version from the module
line, so the directory does not have to repeat it.

```
p/alice/md/gnomod.toml     module = "gno.land/p/alice/md/v1"
p/alice/md/md.gno          edited in place
```

A bump is one line, and then the real diff:

<p align="center">
  <img src="docs/img/bump.svg" alt="gnopm bump changes one line in gnomod.toml, and git records a real diff" width="760">
</p>

Versions that no longer have a directory are pinned in `gnomod.lock` to the
commit that still holds them, and rebuilt into a gitignored `.gnopm/` on
demand, so anything still importing `.../md/v0` keeps resolving.

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

`status` to ask and `sync` to fix are the two you type. `<package>` is a
directory, a module path, or any unambiguous part of one, and with no argument
`bump` uses the package you are standing in. Data goes to stdout and nothing
else does, so it pipes:

```sh
gnopm ls -q | xargs -n1 gno lint
gnopm status -json | jq -e .ok
```

Migrating a repository that still has versioned directories is one command,
and it shows you the plan first:

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

One entry per resolvable module path. `hash` is `h1:`, the same construction as
a `go.sum` line. It is **source, not a generated artifact**: a change that bumps
a version carries the pin that keeps the old one resolvable, or CI cannot build
what still imports it. It changes only on add, remove or bump.

Named after the manifest rather than the tool, and at the workspace root only,
because that is what every ecosystem does: `Cargo.toml`/`Cargo.lock`,
`package.json`/`package-lock.json`, `Gemfile`/`Gemfile.lock`.

**Pins go to a commit already on the default branch.** Most repositories
squash-merge, so a version pinned to a branch commit stops resolving the moment
the change lands. `verify` catches that while it is still cheap, and CI needs
`fetch-depth: 0` because a shallow clone has none of the pinned commits.

## As a library

| import | what |
|---|---|
| [`moul.io/gnopm/pkg/gnomodlock`](./pkg/gnomodlock) | the `gnomod.lock` format, with no dependency on the CLI |
| [`moul.io/gnopm/pkg/gnopm`](./pkg/gnopm) | the operations: `Sync`, `Bump`, `Verify`, `Tidy`, `Deversion` |

No third-party dependencies anywhere. The repository root is the command and a
high-level integration test, nothing else.

## More

- **[moul/gnopm-demo](https://github.com/moul/gnopm-demo)** is a generated
  worked example. Read it as a history: `git log --reverse --stat`.
- [`scripts/demo.sh`](./scripts/demo.sh) builds it, and is also the integration
  test. A demo that is not executed rots; one that is executed as a test
  cannot. The terminal images above come from real runs the same way.
- [Roadmap and design notes](https://github.com/moul/gnopm/issues/2) ·
  [Migrating a repository](https://github.com/moul/gnopm/issues/3) ·
  [CONTRIBUTING](./CONTRIBUTING.md)

## License

© 2026 [Manfred Touron](https://moul.io). Apache-2.0 or MIT, at your option.
See [`COPYRIGHT`](./COPYRIGHT).
