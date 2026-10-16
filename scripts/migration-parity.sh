#!/usr/bin/env bash
# migration-parity.sh [--allow-ahead] <ref> <other ref>
#
# The two editions must ship the same migrations: the same files under
# migrations/, each with the same SHA-384 (the checksum sqlx records when it
# applies one). A workspace can move between the AI edition (main) and the
# AI-free one (without-ai), and sqlx refuses a database that holds a migration
# the new build lacks, or one whose bytes changed after it ran. So a migration
# on one line and not the other breaks that move at `make migrate_up`. The
# lines drifted twice before this check existed: four migrations missing on
# 9 Oct 2026, and 214 on 15 Oct, both found by hand.
#
# Exit 0 when <ref> and <other ref> hold the same files with the same SHA-384.
# With --allow-ahead, <ref> may also hold files <other ref> doesn't (a line
# that is pushed first, or beta, which carries migrations before they ship);
# every file <other ref> has must still be in <ref>, unchanged.
# Exit 1 on any difference, naming each file; exit 2 on a usage error or a ref
# with no migrations at all (a check that compares nothing must not pass).
#
# Runs in the release gate (the push script checks its pinned heads) and in CI
# on main, without-ai and beta (.github/workflows/ci.yml, job "editions").
set -euo pipefail

ahead=0
if [ "${1:-}" = --allow-ahead ]; then ahead=1; shift; fi
if [ $# -ne 2 ]; then
  echo "usage: migration-parity.sh [--allow-ahead] <ref> <other ref>" >&2
  exit 2
fi
a=$1 b=$2
for ref in "$a" "$b"; do
  git rev-parse -q --verify "$ref^{commit}" >/dev/null || { echo "migration-parity: no commit named $ref" >&2; exit 2; }
done
name() { printf '%s (%s)' "$1" "$(git rev-parse --short "$1^{commit}")"; }

sha384() {
  if command -v sha384sum >/dev/null; then sha384sum | cut -d' ' -f1
  elif command -v shasum >/dev/null; then shasum -a 384 | cut -d' ' -f1
  else openssl dgst -sha384 -r | cut -d' ' -f1
  fi
}

# manifest <ref>: "<path> <sha384>" for every file under migrations/, by path.
manifest() {
  git ls-tree -r "$1" -- migrations/ | while IFS=$'\t' read -r meta path; do
    set -- $meta # mode type object
    [ "$2" = blob ] || continue
    printf '%s %s\n' "$path" "$(git cat-file blob "$3" | sha384)"
  done | LC_ALL=C sort
}

ma=$(manifest "$a")
mb=$(manifest "$b")
for pair in "$a:$ma" "$b:$mb"; do
  if [ -z "${pair#*:}" ]; then echo "migration-parity: $(name "${pair%%:*}") holds no migrations; nothing to compare" >&2; exit 2; fi
done

only_a=$(LC_ALL=C join -v1 <(printf '%s\n' "$ma") <(printf '%s\n' "$mb") | cut -d' ' -f1)
only_b=$(LC_ALL=C join -v2 <(printf '%s\n' "$ma") <(printf '%s\n' "$mb") | cut -d' ' -f1)
changed=$(LC_ALL=C join <(printf '%s\n' "$ma") <(printf '%s\n' "$mb") | awk '$2 != $3 { print $1 " (" substr($2, 1, 12) "… against " substr($3, 1, 12) "…)" }')

# versions <manifest or paths>: the migration versions, as numbers (as sqlx reads them).
versions() { printf '%s\n' "$1" | sed -n 's#^migrations/\([0-9][0-9]*\)_.*#\1#p' | awk '{ print $1 + 0 }' | sort -n -u; }
count=$(printf '%s\n' "$mb" | wc -l | tr -d ' ')
range="versions $(versions "$mb" | head -1) to $(versions "$mb" | tail -1)"

fail=0
if [ -n "$only_b" ]; then
  fail=1
  echo "only in $(name "$b"):"; printf '  %s\n' $only_b
fi
if [ -n "$only_a" ]; then
  if [ "$ahead" = 1 ]; then
    echo "ahead in $(name "$a"), allowed: versions $(versions "$only_a" | tr '\n' ' ')"
  else
    fail=1
    echo "only in $(name "$a"):"; printf '  %s\n' $only_a
  fi
fi
if [ -n "$changed" ]; then
  fail=1
  echo "the same file with different bytes (SHA-384):"; printf '%s\n' "$changed" | sed 's/^/  /'
fi
if [ "$fail" = 1 ]; then
  echo "migration-parity: FAIL. $(name "$a") and $(name "$b") must ship the same migrations, byte for byte:"
  echo "  a workspace moving between the editions would fail at make migrate_up. Put the same files on both lines."
  exit 1
fi
if [ "$ahead" = 1 ] && [ -n "$only_a" ]; then
  echo "migration-parity: ok. $(name "$a") holds all $count migration files of $(name "$b") ($range), each with the same SHA-384, and more."
else
  echo "migration-parity: ok. $(name "$a") and $(name "$b") hold the same $count migration files ($range), each with the same SHA-384."
fi
