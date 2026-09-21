# Contributing to gnopm

The authoritative guide. [`AGENTS.md`](./AGENTS.md) and [`CLAUDE.md`](./CLAUDE.md) point here.

## Setup

```sh
go test ./...          # everything, including the integration test
go run . help
```

No network, no gno toolchain, no dependencies. The integration test builds its
own git repository in a temporary directory.

`gno` on `PATH` is optional: when present, `scripts/demo.sh` also lints and
tests the workspace it generates, the only check that the generated gno code
compiles.

## Layout

```
main.go                 entry point only, no logic
pkg/gnopm/              the operations: Sync, Bump, Unbump, Verify, Tidy, Deversion, CI
pkg/gnomodlock/         the gnomod.lock format, independent of the CLI
scripts/demo.sh         the integration test, which is also the demo
scripts/screenshots.sh  regenerates docs/img/ from real output
docs/                   the site at moul.github.io/gnopm
```

**Logic belongs in a package.** Adding code to `main.go` is a smell: the root
is the command plus a high-level integration test, so other programs can use
the packages without shelling out.

**`pkg/gnomodlock` stays importable on its own.** A lock format is only a
format if more than one program can read it, so nothing there may depend on the
CLI, a repository layout, or anything outside the standard library.

The reference documentation is [`README.md`](./README.md); direction and
roadmap are in issue #2. Neither is repeated here.

## Seven claims the code keeps making

Break one deliberately, with a reason, or not at all.

1. **Detect, do not ask.** A required flag is friction paid on every
   invocation forever. `verify` finds its own upstream ref; `bump` finds its
   own package from the working directory.
2. **A build never mutates the lock.** Only `sync` and `bump` write.
3. **The lock is source.** A change that bumps a version carries the pin that
   keeps the old one resolvable, or CI cannot build what still imports it.
4. **Comments are not data.** Making the lock's generated header part of its
   canonical form would break every lock in existence over a wording change.
5. **No resolver, ever.** A gno import path contains its version, so there is
   no constraint to solve. Resist anything that reintroduces one.
6. **Not a wallet.** gnopm reads chains and may *generate* a `gnokey` command.
   It never signs, holds a key, or broadcasts.
7. **A chain answer is not a chain failure.** `ABCIQuery` returns `*ABCIError`
   when a node replied and said no, and a plain error when nothing replied.
   Every guard here is built on "absent means it was published to nobody", so
   collapsing the two lets an unreachable node open all of them.

## Style

**Standard library only.** Argue for a dependency in the pull request first.

**Comments explain why.** The code says what. A comment earns its place by
recording the trap avoided, the alternative rejected, or the measurement that
settled it.

**Errors name the fix.** `gnomod.lock is stale: ... Run gnopm sync`. A message
that only states a fact makes the reader do the work twice.

**Output discipline.** Data on stdout and nothing else, so it pipes.
Diagnostics on stderr. **Silent when there is nothing to report**, or the
output becomes noise people stop reading.

**No em dashes**, anywhere. Conventional single-line commits: `feat:`, `fix:`,
`docs:`, `build:`, `test:`, `chore:`.

## Tests

Table-driven where there is a table. They live beside the code in `pkg/`.

**`scripts/demo.sh` is the integration test** and runs under `go test`. It
builds a workspace from nothing, asserts at every step, needs no network, and
is also what produces the demo repository. A demo that is not executed rots;
one executed as a test cannot.

**Every bug gets a regression test in the same change as the fix.** Say in the
test comment what went wrong and why existing tests could not see it. Several
tests here exist because the integration test caught something unit tests
structurally could not reach, and that history is worth keeping legible.

Assert the claims that are about **git**: that a migration is recorded as
renames, that a version with no directory still materializes byte-for-byte,
that a pin lands on a commit which survives a squash merge.

```sh
go test ./... && go vet ./... && gofmt -l .   # the last must print nothing
```

## Docs, screenshots, demo

Part of a change, not follow-up work.

- **`README.md`** is the reference. Behaviour change means a README change.
- **`./scripts/screenshots.sh`** regenerates `docs/img/` from real output. Run
  it when output changes. Never hand-edit an SVG or hand-write output into the
  script: an image that is not a real capture is wrong within a week.
- **`docs/`** is the site. Keep it honest with the README rather than longer.
- **[moul/gnopm-demo](https://github.com/moul/gnopm-demo)** is regenerated,
  never edited: `./scripts/demo.sh /tmp/demo --push moul/gnopm-demo`.

## Issues and pull requests

- **#2** is the meta issue: purpose, roadmap, prior art, open questions. Argue
  there before building something large.
- **#3** tracks migrating repositories onto gnopm; migration pull requests link
  to it instead of re-explaining themselves.
- Small issues for units of work. Link the pull request to the issue, and say
  **what you decided and why**. The interesting part of a change is usually the
  option you rejected.

## Done means

1. `go test ./...`, `go vet ./...`, `gofmt -l .` clean.
2. A regression test exists if this was a bug.
3. README updated if behaviour changed; screenshots rerun if output changed.
4. The pull request says what was decided and why.

## Reporting a bug

What you ran, what happened, what you expected. `gnopm env` prints everything
gnopm detected rather than was told, usually the difference between a
five-minute answer and an afternoon. Best of all is a recipe that reproduces it
from nothing, in the shape `scripts/demo.sh` uses.
