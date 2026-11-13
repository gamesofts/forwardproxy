#!/usr/bin/env bash
set -euo pipefail
: "${CADDY_VERSION:?CADDY_VERSION is required}"

# Exercise the plugin with the selected Caddy dependency without editing the fork.
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
git archive HEAD | tar -xf - -C "$test_dir"
cd "$test_dir"
go get "github.com/caddyserver/caddy/v2@$CADDY_VERSION"
go mod tidy
go test -mod=readonly -race ./...
