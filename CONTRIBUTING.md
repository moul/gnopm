# Contributing to gnopm

The authoritative guide for anyone, human or agent, working in this repository.
[`AGENTS.md`](./AGENTS.md) and [`CLAUDE.md`](./CLAUDE.md) point here.

## Getting set up

```sh
git clone https://github.com/moul/gnopm && cd gnopm
go test ./...          # everything, including the integration test
go run . help
```

Nothing else is required: no network, no gno toolchain, no dependencies. The
integration test builds its own git repository in a temporary directory.

`gno` on your `PATH` is optional. When it is there, `scripts/demo.sh`
additionally lints and tests the workspace it generates, which is the only
check that the generated gno code actually compiles.

## What this is

A package manager for gno workspaces. The version of a package lives in its
`gnomod.toml` module line, never in its directory name. `gnomod.lock` records
where each version's source actually is, and `.gnopm/` rebuilds the versions
that no longer have a directory.

The reference documentation is [`README.md`](./README.md). Direction, open
questions and the roadmap are in **issue #2**. Do not duplicate either here.

## Layout

```
main.go              the command. Entry point only, no logic
pkg/gnopm/           the operations: Sync, Bump, Verify, Tidy, Deversion
pkg/gnomodlock/      the gnomod.lock format, with no dependency on the CLI
scripts/demo.sh      the integration test, which is also the demo
scripts/termsvg.py   renders captured terminal output to SVG
scripts/screenshots.sh  regenerates docs/img/ from real output
docs/                the site published at moul.github.io/gnopm
```

**Logic belongs in a package.** If you are adding code to `main.go`, stop: the
root is the command and a high-level integration test, so that other programs
can use the packages without shelling out.

`pkg/gnomodlock` must stay importable on its own. A lock format is only a
format if more than one program can read it, so nothing in there may depend on
the CLI, on a repository layout, or on anything outside the standard library.

## Six claims the code has to keep making

Break one of these deliberately, with a reason, or not at all.

1. **Detect, do not ask.** If gnopm can work something out, it works it out. A
   flag is an admission that it could not, and every required flag is friction
   paid on every invocation forever. `verify` finds its own upstream ref;
   `bump` finds its own package from the working directory.
2. **A build never mutates the lock.** Only `sync` and `bump` write.
3. **The lock is source, not a generated artifact.** A change that bumps a
   version carries the pin that keeps the old version resolvable, or CI cannot
   build what still imports it.
4. **Comments are not data.** Prose in a generated file must never be
   load-bearing. Making the lock's header part of its canonical form would
   break every lock in existence over a wording change.
5. **No resolver, ever.** A gno import path contains its version, so there is
   no transitive constraint to solve. Resist any feature that reintroduces one.
6. **Not a wallet.** gnopm reads chains and may *generate* a `gnokey` command
   for a human to run. It never signs, never holds a key, never broadcasts.

## Style

**Go, standard library only.** A lockfile format whose reference
implementation needs a dependency tree is a bad advertisement for itself. If
you think you need a dependency, say why in the pull request first.

**Comments explain why, not what.** The code says what it does. A comment earns
its place by recording the thing that is not obvious: the trap avoided, the
alternative rejected, the measurement that settled it. Write them where the
decision lives, not in a separate document that will drift.

**Errors say what to run next.** Every failure the user can fix should name the
command that fixes it. `gnomod.lock is stale: ... Run gnopm sync` is the
shape. A message that only states a fact makes the reader do the work twice.

**Output discipline.** Data goes to stdout and nothing else does, so everything
pipes. Progress, warnings and diagnostics go to stderr. A command that is safe
to run constantly must be **silent when there is nothing to report**, or its
output becomes noise people stop reading.

**No em dashes**, anywhere: code, comments, docs, commit messages, pull request
bodies. Comma, colon, parentheses, or split the sentence.

**Commits** are conventional and single-line: `feat:`, `fix:`, `docs:`,
`build:`, `test:`, `chore:`, optionally scoped. The body is for why, if the
subject cannot carry it.

## Tests

**Unit tests are table-driven** where there is a table to drive. They live
beside the code, in `pkg/`.

**`scripts/demo.sh` is the integration test**, and it runs under `go test`. It
builds a whole workspace from nothing, asserts at every step, and needs no
network. It is also what produces the demo repository, deliberately: a demo
that is not executed rots, and one that is executed as a test cannot.

**Every bug gets a regression test, in the same change as the fix.** Name the
test after the behaviour, and say in its comment what went wrong and why the
existing tests could not see it. Several of the tests here exist because the
integration test caught something unit tests structurally could not reach; that
history is worth keeping legible.

Assert the things that are about **git**, not just about Go: that a migration
is recorded as renames, that a version with no directory still materializes
byte-for-byte, that a pin lands on a commit which survives a squash merge.
Those are the claims that matter and the ones a pure unit test cannot make.

```sh
go test ./...          # everything, including the integration test
go vet ./...
gofmt -l .             # must print nothing
```

## Docs, screenshots and the demo repository

These are part of a change, not follow-up work.

- **`README.md`** is the reference. If you change behaviour, change it in the
  same pull request.
- **Screenshots**: `./scripts/screenshots.sh` regenerates `docs/img/` from real
  command output. Run it whenever output changes. Never hand-edit an SVG, and
  never hand-write output into the script: if an image is not a real capture it
  will be wrong within a week.
- **The site** in `docs/` is published at
  [moul.github.io/gnopm](https://moul.github.io/gnopm/). Keep it honest with
  the README rather than longer than it.
- **[moul/gnopm-demo](https://github.com/moul/gnopm-demo)** is regenerated,
  never edited: `./scripts/demo.sh /tmp/demo --push moul/gnopm-demo`. Refresh
  it when the story it tells changes.

## Issues and pull requests

- **#2 is the meta issue**: what gnopm is for, the roadmap, the prior art worth
  stealing from, the open questions. Argue there before building something
  large.
- **#3 tracks migrating existing repositories** onto gnopm. Migration pull
  requests link to it so they do not have to re-explain themselves.
- Small, specific issues for units of work. Link the pull request to the issue
  it closes, and say in the pull request **what you decided and why**, not just
  what you changed. The interesting part of a change is usually the option you
  rejected.
- A pull request that changes behaviour without touching tests or docs is
  incomplete.

## Definition of done

1. `go test ./...`, `go vet ./...` and `gofmt -l .` are clean.
2. A regression test exists if this was a bug.
3. README updated if behaviour changed; `scripts/screenshots.sh` rerun if
   output changed.
4. The pull request says what was decided and why.

## Reporting a bug

Say what you ran, what happened, and what you expected. `gnopm env` prints
everything gnopm detected rather than was told, which is usually the difference
between a five-minute answer and an afternoon.

If it involves a workspace, the most useful thing you can attach is a recipe
that reproduces it from nothing, in the shape `scripts/demo.sh` already uses.
