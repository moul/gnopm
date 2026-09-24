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

# 8. tidy, offline: the pass that finds a version number spent on nothing.
#    -offline because this script runs with no network, by the same rule that
#    keeps the demo a real integration test.
{ echo '$ gnopm tidy -offline'; gnopm tidy -offline 2>&1 || true; } \
  | svg "gnopm tidy -offline" tidy.svg

ls -1 "$out"
