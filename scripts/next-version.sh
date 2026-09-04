#!/usr/bin/env bash
# The next version this repository should release, decided by the commits
# since the last one.
#
# Every commit on main is a squashed pull request whose title this repo
# already requires to be `type(scope): subject`, so the history is a
# conventional-commit log without anyone maintaining it as one. That is what
# makes deciding the number mechanical rather than a judgement call somebody
# has to remember to make.
#
#   feat            a minor bump
#   fix, perf       a patch bump
#   anything with ! before the colon, or BREAKING CHANGE: in the body
#                   see "Before 1.0" below
#   everything else no release at all
#
# docs, chore, ci, test, refactor, style and build change nothing a user of
# the binary can observe, so they do not earn a version. A release nobody can
# tell apart from the last one is noise in the changelog and a download for
# clients who gain nothing by taking it.
#
# Before 1.0 (where this repository is), a breaking change bumps the MINOR.
# 0.y.z is the documented place for a project whose public API is not yet
# stable, and going to 1.0.0 is a statement about stability that a script
# reading commit subjects has no business making. Ask for it by hand:
# `workflow_dispatch` with bump=major.
#
# Writes `key=value` lines to stdout, for $GITHUB_OUTPUT. Reads nothing but
# git, so it runs the same on a laptop as in Actions:
#
#   scripts/next-version.sh
set -euo pipefail

# The newest release tag by version order, not by date: a patch cut from an
# older branch must not become the base the next number counts from.
previous="$(git tag --list 'v[0-9]*' --sort=-v:refname | head -n 1)"

if [ -z "$previous" ]; then
  # A repository with no release yet starts here rather than at 0.0.1, so the
  # first automated release is not a special case anybody has to reason about.
  range=""
  major=0 minor=1 patch=0
  first=true
else
  range="${previous}..HEAD"
  first=false
  v="${previous#v}"
  IFS=. read -r major minor patch <<<"$v"
fi

# --no-merges because a merge commit carries no subject anyone wrote: this
# repository squash-merges, so the squashed commit is the pull request, and
# the merge commits in the history predate that and say nothing useful.
subjects="$(git log --no-merges --format='%s' ${range:+$range})"
bodies="$(git log --no-merges --format='%B' ${range:+$range})"

# Collected as flags and decided afterwards, so precedence is stated once
# rather than depending on the order commits happen to be read in.
breaking=false feature=false patchable=false
while IFS= read -r s; do
  [ -n "$s" ] || continue
  # type(scope)!: or type!: — the ! marks the break, and it sits before the
  # colon, so a subject merely containing one does not count.
  if [[ "$s" =~ ^[a-z]+(\([^\)]*\))?!: ]]; then
    breaking=true
    continue
  fi
  case "$s" in
    feat:*|feat\(*\):*)                       feature=true ;;
    fix:*|fix\(*\):*|perf:*|perf\(*\):*)     patchable=true ;;
  esac
done <<<"$subjects"

# A footer anywhere in a body breaks, whatever its subject claimed.
if grep -qE '^BREAKING[ -]CHANGE:' <<<"$bodies"; then
  breaking=true
fi

if [ "$breaking" = true ]; then
  bump=major
elif [ "$feature" = true ]; then
  bump=minor
elif [ "$patchable" = true ]; then
  bump=patch
else
  bump=none
fi

if [ "$first" = true ]; then
  # Nothing to compare against; the seed version above stands, but only if
  # something releasable is actually here.
  [ "$bump" = none ] && { echo "release=false"; echo "bump=none"; echo "previous="; exit 0; }
  echo "release=true"; echo "bump=$bump"; echo "previous="; echo "version=v0.1.0"; exit 0
fi

case "$bump" in
  none)
    echo "release=false"
    echo "bump=none"
    echo "previous=$previous"
    exit 0
    ;;
  major)
    if [ "$major" -eq 0 ]; then
      # See "Before 1.0" above: a break moves the minor, and 1.0.0 stays a
      # decision a person makes.
      minor=$((minor + 1)); patch=0; bump="minor (breaking, pre-1.0)"
    else
      major=$((major + 1)); minor=0; patch=0
    fi
    ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac

echo "release=true"
echo "bump=$bump"
echo "previous=$previous"
echo "version=v${major}.${minor}.${patch}"
