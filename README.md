<p align="center">
  <img src="docs/img/logo.svg" alt="" width="104">
</p>

<h1 align="center">gnopm</h1>

<p align="center">
  <b>A package manager for <a href="https://gno.land">gno</a> workspaces.</b><br>
  The version of a package lives in its <code>gnomod.toml</code>, not in its directory name.
</p>

<p align="center">
  <a href="https://github.com/moul/gnopm/actions/workflows/ci.yml"><img src="https://github.com/moul/gnopm/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://pkg.go.dev/moul.io/gnopm"><img src="https://pkg.go.dev/badge/moul.io/gnopm.svg" alt="Go Reference"></a>
  <a href="https://github.com/moul/gnopm/releases/latest"><img src="https://img.shields.io/github/v/release/moul/gnopm?label=release&color=7ee787" alt="Latest release"></a>
  <a href="./COPYRIGHT"><img src="https://img.shields.io/badge/license-Apache--2.0%20%2F%20MIT-%2397ca00.svg" alt="License"></a>
  <a href="https://moul.github.io/gnopm/"><img src="https://img.shields.io/badge/docs-moul.github.io%2Fgnopm-blue" alt="Site"></a>
</p>

```sh
go install moul.io/gnopm@latest        # with Go
```

or take the binary, on a machine with no Go at all:

```sh
curl -sSfL https://github.com/moul/gnopm/releases/latest/download/gnopm_linux_x86_64.tar.gz \
  | tar -xz gnopm && sudo mv gnopm /usr/local/bin/
```

Then:

```sh
gnopm status      # ask
gnopm sync        # fix
```

No dependencies, no gno toolchain required, no network required. One binary,
and [every release](https://github.com/moul/gnopm/releases) carries it built for
linux, macOS and Windows on amd64 and arm64, with checksums.

---

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
them, and rebuilt into a `.gnopm/` on demand, so anything importing `.../md/v0`
keeps resolving.

> **In production.** [`moul/gno-contracts`](https://github.com/moul/gno-contracts)
> runs on this: 285 module paths in one lock, **42 of them with no directory at
> all**, still linted, still tested, still publishable. Their pins have survived
> that repository's own squash merges. Checked 2026-09-24.

## The commands

<p align="center">
  <img src="docs/img/help.svg" alt="gnopm help: eighteen commands, grouped" width="800">
</p>

`status` to ask, `sync` to fix, and those two are most of it. `sync` converges:
whatever it tells you to run next, running it leaves nothing to say. A version
that has left the tree, because you deleted its directory or edited its module
line by hand, is pinned to the commit that still holds its source, the way
`bump` would have pinned it, and on the default branch where one exists so a
squash merge cannot strand it.

<p align="center">
  <img src="docs/img/status.svg" alt="gnopm status" width="560">
</p>

`<package>` is a directory, a module path, or any unambiguous part of one; with
no argument `bump` uses the package you are standing in. Data goes to stdout and
nothing else does, so it pipes:

```sh
gnopm ls -q | xargs -n1 gno lint
gnopm status -json | jq -e .ok
gnopm ls -f '{{.Module}} {{.Commit}}'   # a go-template, as `gno list -f` does
```

`-f` takes gno's flag name and gno's semantics on purpose: one template per
record, a newline after each, and refusing to be combined with `-json`. It works
on every command that takes `-json`, and the struct a template names is the
struct the JSON carries, so a field cannot exist in one view and not the other.

## `gnopm why`, and the importer with no directory

`why` is the question `bump` raises and the one `tidy` answers silently: before
dropping a pinned version, what would stop resolving?

<p align="center">
  <img src="docs/img/why.svg" alt="gnopm why finds an importer that lives only in the materialized assembly" width="760">
</p>

It searches the materialized assembly as well as the working tree, because a
superseded version importing an older one is how a chain of versions stays
alive, and that importer has no directory for grep to find. Grep would tell you
the version is safe to drop.

## A version number is a tag, not a counter

A number only means something once the version is published: until then it names
no bytes, resolves for no importer, and derives no address. So the rule is **bump
when the last version shipped, and edit in place when it did not.**

Getting it wrong is the default behaviour of a stack of pull requests. The first
bumps `v0` to `v1` and lands. The second is written against it, sees `v1` taken,
and goes to `v2`. Both merge before either is published, and the package reaches
a chain as `v2` with `v1` existing nowhere. Often the lock cannot even show it:
`bump` will not pin a version living on no commit the default branch has, so the
number is skipped rather than recorded and the lock jumps `v0` to `v2` with
nothing in between.

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

<p align="center">
  <img src="docs/img/tidy.svg" alt="gnopm tidy -offline" width="620">
</p>

Four passes: `sync`, then drop any pin nothing imports whose commit never
reached the default branch, then `verify`'s expensive proof over what is left,
then the chain. Against a live chain the fourth pass is the interesting one:

```
4 version number(s) spent on nothing:
  gno.land/r/moul/x/daily/todos/v2
    v1 is free: nothing published has ever taken that number.
    gnopm unbump r/moul/x/daily/todos lowers it to v1.
```

It writes only what is safe to write unasked; lowering a version changes a
package's identity, so that stays a deliberate `unbump`. `-offline` skips the
chain pass, `-n` changes nothing.

## Publishing

`publish` reads the chain your paths point at, says what is **live**, **parked**
or **absent** there, and publishes the rest in dependency order. It holds no key
and signs nothing: `gnokey` does, with your terminal attached, and it prompts you
for the passphrase exactly as it would if you had typed the command.

```sh
gnopm publish                # the whole workspace
gnopm publish r/moul/reaper  # one package, and what it imports from here
gnopm publish -print         # write the commands out, run nothing
gnopm publish -o tx.json     # one document, one signature, all of it
```

<p align="center">
  <img src="docs/img/publish.svg" alt="gnopm publish: live, parked, absent, and one signature for the rest" width="800">
</p>

`-print` is how you look before you leap. Save what it writes and run the file;
do **not** pipe it into `sh`, because a pipe takes stdin away and gnokey cannot
prompt for the passphrase.

Naming one package plans its in-tree dependencies too, ahead of it: a dependency
you could publish yourself is not a missing dependency. Only an import that is in
neither this workspace nor the chain stops the plan. The chain is read in one
concurrent batch behind a progress bar, so a hundred packages is a few seconds
and not a minute (measured against `gnoland-1` on 2026-09-21: 47s down to 6s over
178 paths).

It works out the chain from the package path (`gno.land/...` asks
`https://gno.land` for its rpc and chain id, so there is no endpoint table), the
key from the namespace in that same path (a namespace is its owner), the order
from each package's non-test imports, and gas, fee and deposit from the real
payload. `-key`, `-rpc` and `-chainid` override any of it, and `-gnokey-cmd`
emits a wrapper instead of plain `gnokey`. Where a chain parks submissions, a
green broadcast is not a deployment, so `parked` is reported as its own state
rather than as success.

### One signature for the whole deploy

The command list is the lowest common denominator: it assumes the signer is a CLI
on this machine, and it is **not atomic**. `set -e` stops at the first failure,
which leaves the dependencies up and the thing that needed them not, a state
neither the tree nor the chain describes. N packages is also N password prompts.

A tm2 transaction carries a list of messages, not one, and `gnokey sign` does not
care how many are in it. So `-o` writes the whole deploy as one unsigned
document:

```sh
gnopm publish -o tx.json -addr g1... | sh   # sign once, broadcast once
```

All the packages land or none do. It is also the shape a multisig ceremony needs,
so a DAO-owned namespace gets the same path for free. gnopm still never signs: it
writes the document and prints the two commands.

`-addr` is needed because the creator is a field of every message and gnopm does
not read your keybase (`gnokey list` shows it). The account number and sequence
are read from the chain and filled into the `gnokey sign` command, with the caveat
that matters: the signature covers both, so the document stops being valid the
moment that account signs anything else. A deploy larger than one transaction
(`MaxTxBytes` is 1,000,000 on `gnoland-1`) is split into `tx.1.json`, `tx.2.json`
and so on, still in dependency order, each taking the next sequence.

### The cache

Asking a chain about two hundred packages is two hundred round trips, so the one
answer that cannot change is kept. A path that is live on a chain stays live:
there is no delete, and `addpkg` on an occupied path fails, so its bytes can never
be redefined either. `~/.gnopm/live/<chain-id>` remembers those, one path per
line, and later runs only ask about what is missing. Measured on a 193-package
workspace against `gnoland-1`: **5.8s** asking the chain everything, **0.67s**
once the cache is warm, same script out.

`parked` and `absent` are never remembered: a parked submission can still be
enabled or rejected, and absent is the state of the very version you are about to
publish. Neither is `chain-id = dev`, which is `gnodev`'s default and belongs to a
chain that gets wiped and restarted under the same name.

```sh
gnopm publish -v          # say what is checked, and whether the chain or the cache answered
gnopm publish -no-cache   # ask the chain everything
GNOPM_CACHE=off gnopm …   # the same, for a whole shell; or point it elsewhere
gnopm env                 # where the cache is, among everything else gnopm worked out
```

## Graphs

```sh
gnopm graph | dot -Tsvg > graph.svg   # the whole workspace
gnopm graph -latest                   # one node per package
gnopm graph -dependents p/alice/md/v0 # `gnopm why`, drawn
gnopm graph -changed auto             # what this branch touched, plus one hop
gnopm graph -svg                      # render here, when graphviz is on PATH
```

DOT is the default rather than a picture, because a default that changes shape
depending on whether graphviz happens to be installed makes `gnopm graph > g.dot`
write two different files on two machines, and CI is exactly where graphviz is
absent. `-svg` asks for the picture and degrades to DOT with one line on stderr
when it cannot have it.

Nodes say what they are: a realm and a package are coloured differently, a version
pinned to history is dashed because it has no directory left, and a module this
workspace only imports is greyed because nothing here can bump or publish it.
`-latest` is an approximation on purpose: an edge onto an older version is redrawn
onto the latest, and a version importing its own predecessor disappears, because
one node per package is the point.

## Completion

```sh
gnopm completion bash > /etc/bash_completion.d/gnopm
gnopm completion zsh  > "${fpath[1]}/_gnopm"
gnopm completion fish > ~/.config/fish/completions/gnopm.fish
gnopm completion                 # detects the shell from $SHELL
```

What earns it is not the command names, it is **package names**: `gnopm bump
md<TAB>` completes out of this workspace's lock, pinned versions included, with
each candidate described by where its source actually is. A static script could
not offer that at all, so all three scripts are thin and ask the binary, which
also means a flag added today completes today.

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

One entry per module path; `hash` is `h1:`, as in `go.sum`. It is **source, not a
generated artifact**: a change that bumps a version carries the pin keeping the
old one resolvable. It changes only on add, remove or bump. Named after the
manifest rather than the tool, at the workspace root only, as every ecosystem
does it.

**Pins go to a commit already on the default branch.** Most repositories
squash-merge, so a pin to a branch commit stops resolving once the change lands.
`verify` catches that early, and it is the failure worth having:

<p align="center">
  <img src="docs/img/verify.svg" alt="gnopm verify rejects a pin a squash merge would strand" width="720">
</p>

CI needs `fetch-depth: 0`, because a shallow clone has none of the pinned commits.

**A conflicted lock resolves mechanically**, so `gnopm merge-lock` does it.
Squash-merging a base branch makes every stacked branch conflict here, and taking
a side loses pins silently: `--ours` drops whatever the base added that this
branch never had an entry for, `sync` cannot restore it because it only carries
over entries the old lock already had, and `verify` then passes, because a lock
that never mentions a version is consistent, just poorer.

```sh
gnopm merge-lock -n    # the resolution, changing nothing
gnopm merge-lock       # write it, git add it, sync
```

Identical wins, one-sided wins, and `{ dir }` against `{ commit, hash }` takes the
pinned one, because the side that pinned a version is the side that bumped past
it. It reads the merge stages out of the index rather than parsing conflict
markers, names what it took from each side, and refuses when one module is pinned
to two different commits, which is the one ambiguous case and is not what a squash
merge produces.

## `.gnopm/`

gnopm creates the assembly directory, so gnopm is the one that keeps it out of
git: the first time it materializes anything it adds `/.gnopm/` to `.gitignore`,
unless git already ignores it some other way. Nothing in there is source, so
`gnopm clean` drops it and `gnopm sync` puts it back byte for byte; `gnopm clean
-cache` also drops the shared chain cache, the way `go clean -cache` does.

## Migrating an existing repository

One command, and it shows the plan first:

```sh
gnopm deversion -n
gnopm deversion
gnopm sync
```

<p align="center">
  <img src="docs/img/deversion.svg" alt="gnopm deversion -n" width="680">
</p>

The whole move is recorded by git as renames. No package path moves, so no realm
address moves. Commit it on its own, so it reviews as a rename list. Context,
review checklist and the repositories done so far:
[#3](https://github.com/moul/gnopm/issues/3).

## As a library

| import | what |
|---|---|
| [`moul.io/gnopm/pkg/gnomodlock`](./pkg/gnomodlock) | the `gnomod.lock` format, with no dependency on the CLI |
| [`moul.io/gnopm/pkg/gnopm`](./pkg/gnopm) | the operations: `Sync`, `Bump`, `Unbump`, `Verify`, `Tidy`, `Deversion` |

No third-party dependencies. The root is the command and a high-level integration
test, nothing else.

## Contributing

**Issues are wanted, and small ones most of all.** If gnopm surprised you, that
is a bug even when it behaved as documented: most of what is in here started as
somebody saying "I expected it to just do that".

- 🐛 [**Report a bug**](https://github.com/moul/gnopm/issues/new?template=bug.yml).
  `gnopm env` prints everything gnopm detected rather than was told, and is usually
  the difference between a five-minute answer and an afternoon.
- 💡 [**Ask for a command or a flag**](https://github.com/moul/gnopm/issues/new?template=idea.yml).
- 🧭 [**#2**](https://github.com/moul/gnopm/issues/2) is the meta issue: goal,
  roadmap, prior art, open questions, and the place to argue before building
  something large. [**#3**](https://github.com/moul/gnopm/issues/3) tracks
  migrating repositories.
- 🟢 [**good first issue**](https://github.com/moul/gnopm/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
  is kept stocked, and each one says what "done" means.

`make` runs exactly what CI runs: tests, `go vet`, `gofmt`, `staticcheck`, and the
demo. No network, no gno toolchain, no dependencies. Read
[CONTRIBUTING](./CONTRIBUTING.md) before the first pull request; it is short, and
the seven claims the code keeps making are the part worth arguing with.

## More

- **[moul/gnopm-demo](https://github.com/moul/gnopm-demo)** is a generated worked
  example. Read it as a history: `git log --reverse --stat`.
- [`scripts/demo.sh`](./scripts/demo.sh) builds it and is the integration test. A
  demo that is not executed rots; one executed as a test cannot. The terminal
  images above are captured the same way, by
  [`scripts/screenshots.sh`](./scripts/screenshots.sh).
- Full command reference: `gnopm help <command>`, or
  **[moul.github.io/gnopm](https://moul.github.io/gnopm/)**.

## License

© 2026 [Manfred Touron](https://moul.io). Apache-2.0 or MIT, at your option. See
[`COPYRIGHT`](./COPYRIGHT).
