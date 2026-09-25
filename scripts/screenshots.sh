#!/usr/bin/env bash
#
# Regenerate the terminal images in docs/img/ from REAL command output.
#
# Every image in the README and on the site comes from running the tool here,
# so none of them can quietly stop matching what it prints. A screenshot taken
# by hand rots the first time an output line changes and nobody notices; this
# is the same bargain as the demo script being the integration test.
#
# usage: scripts/screenshots.sh [outdir]

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"
out="${1:-$root/docs/img}"
mkdir -p "$out"

bin="$(mktemp -d)/gnopm"
(cd "$root" && go build -o "$bin" .)

repo="$(mktemp -d)/demo"
GNOPM="$bin" "$here/demo.sh" "$repo" >/dev/null 2>&1

gnopm() { "$bin" -C "$repo" "$@"; }
svg() { python3 "$here/termsvg.py" "$1" > "$out/$2"; }

# 1. status: the one command you type to ask.
{ echo '$ gnopm status'; gnopm status 2>&1; } | svg "gnopm status" status.svg

# 2. ls: where every version's source actually is.
{ echo '$ gnopm ls'; gnopm ls 2>&1; } | svg "gnopm ls" ls.svg

# 3. a bump, and what git makes of it. The point of the whole tool.
{
  echo '$ gnopm bump set'
  gnopm bump set 2>&1
  echo ''
  echo '$ git diff --stat'
  git -C "$repo" diff --stat 2>&1 | sed 's/^ //'
} | svg "gnopm bump: one line, not a copied directory" bump.svg
git -C "$repo" checkout -q -- . 2>/dev/null || true

# 4. the guard that matters: a pin a squash merge would discard.
#    Produced for real, by making the mistake it exists to catch: edit a
#    published version, then bump it, so its content exists only on this branch.
strand="$(mktemp -d)/strand"
GNOPM="$bin" "$here/demo.sh" "$strand" >/dev/null 2>&1
git -C "$strand" update-ref refs/remotes/origin/main HEAD
git -C "$strand" checkout -q -b feature
printf '\n// edited before bumping, which is the mistake\n' >> "$strand/p/demo/set/set.gno"
git -C "$strand" add -A
git -C "$strand" -c commit.gpgsign=false commit -q -m "edit set"
"$bin" -C "$strand" bump set >/dev/null 2>&1
git -C "$strand" add -A
git -C "$strand" -c commit.gpgsign=false commit -q -m "bump set"
{ echo '$ gnopm verify'; "$bin" -C "$strand" verify 2>&1 || true; } \
  | svg "gnopm verify: the failure worth having" verify.svg

# 5. the migration, as a plan you can read before running it. Also real: a
#    throwaway workspace built in the OLD layout, one directory per version.
old="$(mktemp -d)/old"
mkdir -p "$old/p/demo/strs/v0" "$old/p/demo/strs/v1" "$old/p/demo/table/v0"
: > "$old/gnowork.toml"
printf '/.gnopm/\n' > "$old/.gitignore"
for d in strs/v0 strs/v1 table/v0; do
  name="${d%%/*}"; ver="${d##*/}"
  printf 'module = "gno.land/p/demo/%s/%s"\ngno = "0.9"\n' "$name" "$ver" > "$old/p/demo/$d/gnomod.toml"
  printf 'package %s\n' "$name" > "$old/p/demo/$d/$name.gno"
done
git -C "$old" init -q -b main
git -C "$old" config user.email demo@example.com
git -C "$old" config user.name "gnopm demo"
git -C "$old" add -A
git -C "$old" -c commit.gpgsign=false commit -q -m "the old layout"
{ echo '$ gnopm deversion -n'; "$bin" -C "$old" deversion -n 2>&1; } \
  | svg "gnopm deversion -n" deversion.svg

# 6. the whole surface, grouped. Eighteen commands in one column was a wall,
#    and this is the first thing anybody who types `gnopm` sees.
{ echo '$ gnopm help'; "$bin" help 2>&1; } | svg "gnopm help" help.svg

# 7. why, on the case that is the whole reason it exists. strs/v0's only
#    importer is table/v0, which has no directory at all: it is pinned to
#    history and materialized under .gnopm. grep over the working tree finds
#    nothing, and would tell you the version is safe to drop.
{
  echo '$ gnopm why gno.land/p/demo/strs/v1'
  gnopm why gno.land/p/demo/strs/v1 2>&1
  echo ''
  echo '$ gnopm why gno.land/p/demo/strs/v0'
  gnopm why gno.land/p/demo/strs/v0 2>&1
  echo ''
  echo '# table/v0 has no directory. It is pinned to history:'
  echo '$ gnopm ls'
  gnopm ls 2>&1 | grep -E 'MODULE|table'
} | svg "gnopm why: the importer with no directory" why.svg

# 7b. doc, on the thing `go doc` structurally cannot do: document a version
#     that has no directory anywhere, and put the two signatures of a bump next
#     to each other. That IS the compatibility diff.
{
  echo '$ gnopm doc p/demo/strs/v1'
  gnopm doc p/demo/strs/v1 2>&1
  echo ''
  echo '$ gnopm doc gno.land/p/demo/table/v0 Rule'
  gnopm doc gno.land/p/demo/table/v0 Rule 2>&1
  echo ''
  echo '$ gnopm doc gno.land/p/demo/table/v1 Rule'
  gnopm doc gno.land/p/demo/table/v1 Rule 2>&1
} | svg "gnopm doc: the bump, as the two signatures side by side" doc.svg

# 8. tidy, offline: the pass that finds a version number spent on nothing.
#    -offline because this script runs with no network, by the same rule that
#    keeps the demo a real integration test.
{ echo '$ gnopm tidy -offline'; gnopm tidy -offline 2>&1 || true; } \
  | svg "gnopm tidy -offline" tidy.svg

# 9. publish, the command most worth showing and the only one that reads a
#    chain. scripts/fakechain answers the two ABCI queries it makes, so this
#    stays offline like everything else here: a capture taken once against
#    mainnet would be wrong the first time the report's wording changed.
#
#    The port is pinned rather than picked, because a random one would put a
#    different number in the committed svg on every run. 26657 is tm2's own
#    default, so the image also reads as a real chain.
chain_port=26657
if command -v nc >/dev/null 2>&1 && nc -z 127.0.0.1 "$chain_port" 2>/dev/null; then
  echo "port $chain_port is in use, so the publish capture would not be reproducible." >&2
  echo "stop whatever is on it and re-run." >&2
  exit 1
fi
# Built, not `go run`: `go run` execs the compiled binary as a child, so
# killing the go process leaves the listener holding the port and the next run
# trips the guard above. The script already builds gnopm this way for the same
# class of reason.
chain_bin="$(mktemp -d)/fakechain"
(cd "$root" && go build -o "$chain_bin" ./scripts/fakechain)
"$chain_bin" \
  -addr "127.0.0.1:$chain_port" \
  -live gno.land/p/nt/tinyavl/v0,gno.land/p/demo/table/v0,gno.land/p/demo/strs/v0,gno.land/p/demo/strs/v1 \
  -parked gno.land/p/demo/orphan/v0 >/dev/null 2>&1 &
chain_pid=$!
trap 'kill "$chain_pid" 2>/dev/null || true' EXIT
# Wait for the listener rather than sleeping a guess.
for _ in $(seq 1 50); do
  (exec 3<>/dev/tcp/127.0.0.1/"$chain_port") 2>/dev/null && break
  sleep 0.1
done

#    The report only. publish also writes the gnokey commands, and a picture
#    that includes twenty lines of shell says less than one that fits.
{
  echo '$ gnopm publish -o tx.json -addr g1jg8...sqf5'
  GNOPM_CACHE=off gnopm publish -o tx.json \
    -addr g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5 \
    -rpc "http://127.0.0.1:$chain_port" -chainid test 2>&1 |
    sed '/^#!\/bin\/sh/,$d'
  echo '# (it also writes the two gnokey commands, cut here)'
} | svg "gnopm publish: live, parked, absent, and one signature for the rest" publish.svg
# -o is resolved against the process working directory, not against -C, so the
# document lands here rather than in the demo repository.
rm -f "$root/tx.json"
kill "$chain_pid" 2>/dev/null || true
trap - EXIT

ls -1 "$out"
