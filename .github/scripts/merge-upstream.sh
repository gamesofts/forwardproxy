#!/usr/bin/env bash
set -euo pipefail

: "${BASE_BRANCH:?BASE_BRANCH must name the destination branch}"
: "${SYNC_BRANCH:?SYNC_BRANCH must name the dedicated sync branch}"
: "${UPSTREAM_URL:?UPSTREAM_URL is required}"

git fetch origin "+refs/heads/$BASE_BRANCH:refs/remotes/origin/$BASE_BRANCH"
git fetch "$UPSTREAM_URL" master
upstream_sha=$(git rev-parse FETCH_HEAD)
printf 'upstream_sha=%s\n' "$upstream_sha" >> "$GITHUB_OUTPUT"

if git merge-base --is-ancestor "$upstream_sha" "origin/$BASE_BRANCH"; then
  echo 'changed=false' >> "$GITHUB_OUTPUT"
  echo 'The default branch already contains upstream master.' >> "$GITHUB_STEP_SUMMARY"
  exit 0
fi

# Reuse an existing sync branch without discarding human conflict resolutions.
# All pushes are fast-forwards; never rewrite the default or sync branch.
remote_branch=$(git ls-remote --heads origin "refs/heads/$SYNC_BRANCH")
if [[ -n "$remote_branch" ]]; then
  git fetch origin "+refs/heads/$SYNC_BRANCH:refs/remotes/origin/$SYNC_BRANCH"
  git switch -C "$SYNC_BRANCH" "origin/$SYNC_BRANCH"
else
  git switch -c "$SYNC_BRANCH" "origin/$BASE_BRANCH"
fi

merge_ref() {
  if ! git merge --no-ff --no-edit "$1"; then
    {
      echo '## Manual conflict resolution required'
      echo 'No changes have been pushed. Resolve these files on the sync branch:'
      git diff --name-only --diff-filter=U
    } >> "$GITHUB_STEP_SUMMARY"
    git merge --abort
    exit 1
  fi
}

merge_ref "origin/$BASE_BRANCH"
merge_ref "$upstream_sha"
echo 'changed=true' >> "$GITHUB_OUTPUT"
