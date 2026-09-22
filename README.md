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
`.../md/v0` keeps resolving. Nothing in there is source, so `gnopm clean` drops
it and `gnopm sync` puts it back byte for byte; `gnopm clean -cache` also drops
the shared chain cache, the way `go clean -cache` does.

## Use

```sh
gnopm status      # what resolves, and whether anything is out of date
gnopm sync        # make the state good
gnopm bump md     # promote a package, in place; then edit the files
gnopm unbump md   # take a number back, while nothing has published it
gnopm ls          # every module and where its source is
gnopm why md/v0   # who still imports this version
gnopm verify      # prove every pinned version still reproduces (for CI)
gnopm tidy        # make the whole workspace right, chain included
gnopm publish     # what is missing on chain, as gnokey commands you can read
gnopm clean       # drop the assembly; sync rebuilds it
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
gnopm why p/alice/md/v0 -q | wc -l
```

`why` is the question `bump` raises and the one `tidy` answers silently: before
dropping a pinned version, what would stop resolving? It searches the
materialized assembly as well as the working tree, because a superseded version
importing an older one is how a chain of versions stays alive, and that
importer has no directory for grep to find.

## A version number is a tag, not a counter

A number only means something once the version is published: until then it
names no bytes, resolves for no importer, and derives no address. So the rule
is **bump when the last version shipped, and edit in place when it did not.**

Getting it wrong is the default behaviour of a stack of pull requests. The
first bumps `v0` to `v1` and lands. The second is written against it, sees `v1`
taken, and goes to `v2`. Both merge before either is published, and the package
reaches a chain as `v2` with `v1` existing nowhere. Often the lock cannot even
show it: `bump` will not pin a version living on no commit the default branch
has, so the number is skipped rather than recorded and the lock jumps `v0` to
`v2` with nothing in between.

`bump` itself stays offline and literal, because a tool that phones home before
doing what it was told is a tool you cannot use in CI. Asking it to decide is
opt-in, and costs one chain read:

```sh
gnopm bump md -if-published    # bumps only if v0 is live or parked; otherwise says so and exits 0
gnopm unbump md                # the inverse: lower to the lowest number nothing has taken
```

`unbump` reads its target off the chain rather than out of the lock, which is
what makes it safe rather than merely guarded: the target is by construction a
number the chain has never seen, so it cannot withdraw a published version and
cannot redefine one either.

`tidy` is the command that finds all of this for you. Where `sync` is cheap,
silent and idempotent, the one a Makefile prerequisite calls, `tidy` is the one
you run when you want the workspace right and it may cost git walks and chain
reads:

```
$ gnopm tidy -n
lock         up to date
pins         nothing to drop
gnopm: ok, 228 modules locked (193 in tree, 35 pinned to history)
chain        gnoland-1 (https://rpc.gno.land)
             153 live, 0 parked, 40 absent

4 version number(s) spent on nothing:
  gno.land/r/moul/x/daily/todos/v2
    v1 is free: nothing published has ever taken that number.
    gnopm unbump r/moul/x/daily/todos lowers it to v1.
```

Four passes: `sync`, then drop any pin nothing imports whose commit never
reached the default branch, then `verify`'s expensive proof over what is left,
then the chain. It writes only what is safe to write unasked; lowering a
version changes a package's identity, so that stays a deliberate `unbump`.
`-offline` skips the chain pass, `-n` changes nothing.

## Migrating

Migrating a repository off versioned directories is one command, and it shows
the plan first:

```sh
gnopm deversion -n
gnopm deversion
```

`publish` reads the chain your paths point at, says what is **live**, **parked**
or **absent** there, and writes the `gnokey` commands for the rest in dependency
order. It never signs: the report goes to stderr and the script to stdout, so
generating a plan is always safe and running it is a second, deliberate step.

```sh
gnopm publish                # the whole workspace
gnopm publish r/moul/reaper  # one package, and what it imports from here
gnopm publish | sh           # then run it
```

Naming one package plans its in-tree dependencies too, ahead of it: a
dependency you could publish yourself is not a missing dependency. Only an
import that is in neither this workspace nor the chain stops the plan. The
chain is read in one concurrent batch behind a progress bar, so a hundred
packages is a few seconds and not a minute (measured against `gnoland-1` on
2026-09-21: 47s down to 6s over 178 paths).

It works out the chain from the package path (`gno.land/...` asks
`https://gno.land` for its rpc and chain id, so there is no endpoint table),
the key from the namespace in that same path (a namespace is its owner), the
order from each package's non-test imports, and gas, fee and deposit from the
real payload. `-key`, `-rpc` and `-chainid` override any of it, and
`-gnokey-cmd` emits a wrapper instead of plain `gnokey`. Where a chain parks submissions, a green broadcast is not a
deployment, so `parked` is reported as its own state rather than as success.

Asking a chain about two hundred packages is two hundred round trips, so the one
answer that cannot change is kept. A path that is live on a chain stays live:
there is no delete, and `addpkg` on an occupied path fails, so its bytes can
never be redefined either. `~/.gnopm/live/<chain-id>` remembers those, one path
per line, and later runs only ask about what is missing. Measured on a
193-package workspace against `gnoland-1`: **5.8s** asking the chain everything,
**0.67s** once the cache is warm, same script out.

`parked` and `absent` are never remembered: a parked submission can still be
enabled or rejected, and absent is the state of the very version you are about
to publish. Neither is `chain-id = dev`, which is `gnodev`'s default and belongs
to a chain that gets wiped and restarted under the same name.

```sh
gnopm publish -v          # say what is checked, and whether the chain or the cache answered
gnopm publish -no-cache   # ask the chain everything
GNOPM_CACHE=off gnopm …   # the same, for a whole shell; or point it elsewhere
gnopm env                 # where the cache is, among everything else gnopm worked out
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
| [`moul.io/gnopm/pkg/gnopm`](./pkg/gnopm) | the operations: `Sync`, `Bump`, `Unbump`, `Verify`, `Tidy`, `Deversion` |

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
