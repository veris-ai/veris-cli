#!/usr/bin/env bash
# next-version.sh, exercised against real repositories built for each case.
#
# The version this decides is pushed as a tag and released to clients without
# anyone reading it first, so the rules it encodes are worth holding to
# examples rather than to a careful reading of the script. Two of these are
# here because the obvious implementation gets them wrong: a `!` that appears
# AFTER the colon is prose, not a breaking change, and precedence has to hold
# whatever order the commits arrive in.
#
#   bash scripts/next-version-test.sh
set -euo pipefail
S="$(cd "$(dirname "$0")" && pwd)/next-version.sh"
pass=0; fail=0
case_() {  # name, last_tag(or -), subjects..., expect version(or none)
  local name="$1"; shift
  local tag="$1"; shift
  local expect="${@: -1}"
  local subs=("${@:1:$#-1}")
  d=$(mktemp -d); (
    cd "$d"; git init -q .; git config user.email t@t; git config user.name t
    echo x > f; git add f; git commit -qm "chore: seed"
    [ "$tag" != "-" ] && git tag -a "$tag" -m "$tag"
    for s in "${subs[@]}"; do
      echo "$RANDOM" > f; git add f
      printf '%s\n' "$s" | git commit -qF -
    done
    out=$(bash "$S")
    got=$(echo "$out" | grep '^version=' | cut -d= -f2 || true)
    rel=$(echo "$out" | grep '^release=' | cut -d= -f2)
    [ "$rel" = "false" ] && got=none
    if [ "$got" = "$expect" ]; then echo "  PASS  $name -> $got"; else
      echo "  FAIL  $name -> got '$got' want '$expect'"; echo "$out" | sed 's/^/        /'; exit 1; fi
  ) && pass=$((pass+1)) || fail=$((fail+1))
  rm -rf "$d"
}

case_ "docs only earns no release"        v1.2.3 "docs: tidy"                       none
case_ "chore+ci earn no release"          v1.2.3 "chore: bump" "ci: cache"          none
case_ "a fix is a patch"                  v1.2.3 "fix(cli): thing"                  v1.2.4
case_ "perf is a patch"                   v1.2.3 "perf: faster"                     v1.2.4
case_ "a feat is a minor, resetting patch" v1.2.3 "feat(api): thing"                v1.3.0
case_ "feat outranks fix regardless of order" v1.2.3 "fix: a" "feat: b" "fix: c"    v1.3.0
case_ "bang is breaking"                  v1.2.3 "feat(api)!: gone"                 v2.0.0
case_ "bang outranks everything"          v1.2.3 "feat: a" "fix!: b"                v2.0.0
case_ "pre-1.0 breaking moves the minor"  v0.10.2 "feat(cli)!: gone"                v0.11.0
case_ "pre-1.0 feat moves the minor"      v0.10.2 "feat: thing"                     v0.11.0
case_ "pre-1.0 fix moves the patch"       v0.10.2 "fix: thing"                      v0.10.3
case_ "a subject with ! after the colon is not breaking" v1.2.3 "fix: it works now!" v1.2.4
case_ "newest tag wins over commit date"  v1.2.3 "fix: a"                           v1.2.4
echo "passed=$pass failed=$fail"
[ "$fail" = 0 ]
