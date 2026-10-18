#!/bin/sh
# Updates the repository that Vercel deploys your dashboard from to a starkive release.
# That repository is the copy that Vercel created in your GitHub account.
# Run this script in a clone of it (on Windows, in Git Bash).
#
#   sh update.sh           Updates to the latest release
#   sh update.sh v0.2.0    Updates to that release
#
# It adds one commit with the release's files and pushes it, so Vercel redeploys the dashboard.
# The `.github` folder is left as it is.
# The release's `.github` folder holds workflows for starkive itself, not for the dashboard.
#
# NOTE: Each update commit has a `Starkive-Version` trailer.
# The last commit with it marks the last state that matches starkive.
# If there is none, the first commit marks it.
# Later commits are changes made to this copy.
# The update replaces them, so it asks first.
set -eu

UPSTREAM_URL="https://github.com/xorespesp/starkive" # Repository whose releases the update uses
TRAILER="Starkive-Version"

self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

fail() {
  echo "Update stopped: $1" >&2
  exit 1
}

confirm() {
  printf '%s [y/N] ' "$1"
  read -r answer || answer="" # The end of input counts as no
  case "$answer" in
  y | Y | yes) ;;
  *) fail "nothing was changed." ;;
  esac
}

top=$(git rev-parse --show-toplevel 2>/dev/null) || fail "run it in a clone of the repository that Vercel deploys your dashboard from."
cd "$top"

[ -z "$(git status --porcelain --untracked-files=no)" ] || fail "commit or discard your local changes first."

branch=$(git symbolic-ref --short -q HEAD) || fail "check out the default branch first."
default_branch=$(git ls-remote --symref origin HEAD | awk '$1 == "ref:" { sub("refs/heads/", "", $2); print $2; exit }')
[ -n "$default_branch" ] || fail "couldn't read the default branch of origin."
[ "$branch" = "$default_branch" ] || fail "Vercel deploys '$default_branch', so check it out first (now on '$branch')."

git fetch -q origin "$branch"
[ "$(git rev-parse HEAD)" = "$(git rev-parse FETCH_HEAD)" ] || fail "this clone isn't the same as origin/$branch. Pull or push first."

if [ $# -ge 1 ]; then
  tag=$1
else
  # The newest release whose name has no "-" (names with "-" are pre-releases)
  tag=$(git ls-remote --tags --refs --sort=-v:refname "$UPSTREAM_URL" 'v*' | sed 's|.*refs/tags/||' | grep -v -- '-' | head -n 1) || true
  [ -n "$tag" ] || fail "couldn't find a release in $UPSTREAM_URL."
fi
git fetch -q --no-tags "$UPSTREAM_URL" "refs/tags/$tag" 2>/dev/null || fail "couldn't fetch release $tag from $UPSTREAM_URL."
release=$(git rev-parse "FETCH_HEAD^{commit}")

# Runs the release's version of this script if it differs, so the update uses the newest steps.
# `STARKIVE_UPDATE_RERUN` skips this in that run, so it doesn't overwrite the file that `sh` is reading.
if [ -z "${STARKIVE_UPDATE_RERUN:-}" ] && git cat-file -e "$release:update.sh" 2>/dev/null; then
  release_script="$(git rev-parse --absolute-git-dir)/starkive-update.sh"
  git show "$release:update.sh" >"$release_script"
  if ! cmp -s "$release_script" "$self"; then
    STARKIVE_UPDATE_RERUN=1 exec sh "$release_script" "$tag"
  fi
fi

base=$(git log --format="%H %(trailers:key=$TRAILER,valueonly,separator=)" | awk 'NF > 1 { print $1; exit }')
if [ -n "$base" ]; then
  current=$(git log -1 --format="%(trailers:key=$TRAILER,valueonly,separator=)" "$base")
else
  base=$(git rev-list --max-parents=0 HEAD | tail -n 1)
  current="the first deployment"
fi

# `.github` is kept by the update, so changes there don't count
if [ -n "$(git rev-list "$base..HEAD" -- . ':(exclude).github')" ]; then
  echo "This repository was changed after $current:"
  git log --oneline "$base..HEAD" -- . ':(exclude).github'
  git diff --stat "$base" HEAD -- . ':(exclude).github'
  confirm "The update replaces these changes. Continue?"
fi

# Builds the new tree in a temporary index, so the working tree doesn't change until the merge below
index="$(git rev-parse --absolute-git-dir)/starkive-update-index"
rm -f "$index"
GIT_INDEX_FILE=$index git read-tree "$release"
GIT_INDEX_FILE=$index git rm -r -q --cached --ignore-unmatch .github
if git cat-file -e HEAD:.github 2>/dev/null; then
  GIT_INDEX_FILE=$index git read-tree --prefix=.github/ HEAD:.github
fi
tree=$(GIT_INDEX_FILE=$index git write-tree)
rm -f "$index"

if [ "$tree" = "$(git rev-parse "HEAD^{tree}")" ]; then
  echo "Already the same as starkive $tag."
  exit 0
fi

echo "Updating from $current to starkive $tag ($release):"
git diff --stat HEAD "$tree"
confirm "Commit this and push it to origin/$branch?"

# The copy shares no history with starkive, so the commit has `HEAD` as its only parent
commit=$(printf 'chore: Update to starkive %s (%s)\n\n%s: %s (%s)\n' "$(git rev-parse --short=7 "$release")" "$tag" "$TRAILER" "$tag" "$release" | git commit-tree "$tree" -p HEAD)
git merge -q --ff-only "$commit"
git push -q origin "$branch" || fail "the push failed. The update commit is only in this clone. Push it again with 'git push', or undo it with 'git reset --hard origin/$branch'."
echo "Updated to starkive $tag. Vercel redeploys the dashboard in a few minutes."
