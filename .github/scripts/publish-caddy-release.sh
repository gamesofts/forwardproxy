#!/usr/bin/env bash
set -euo pipefail
: "${GITHUB_REPOSITORY:?}"
: "${RELEASE_TAG:?}"
: "${CADDY_VERSION:?}"
: "${SOURCE_SHA:?}"
: "${ARCHIVE:?}"
: "${RELEASE_EXISTS:?}"
: "${MAKE_LATEST:?}"

if [[ "$RELEASE_EXISTS" == false ]]; then
  gh release create "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" \
    --target "$SOURCE_SHA" --title "Caddy $CADDY_VERSION with Naive and Cloudflare (Linux amd64)" \
    --notes-file dist/RELEASE_NOTES.md --draft
fi

# Never replace an already published build, even if a human publishes it mid-run.
release_id=$(gh release view "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --json databaseId --jq '.databaseId')
[[ "$release_id" =~ ^[0-9]+$ ]]
[[ "$(gh api "repos/$GITHUB_REPOSITORY/releases/$release_id" --jq '.draft')" == true ]]
gh release upload "$RELEASE_TAG" "dist/$ARCHIVE" dist/SHA256SUMS \
  --repo "$GITHUB_REPOSITORY" --clobber
gh release edit "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" \
  --target "$SOURCE_SHA" --notes-file dist/RELEASE_NOTES.md --draft=false --latest="$MAKE_LATEST"
