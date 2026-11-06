#!/usr/bin/env bash
set -euo pipefail

# Build the Caddy version tested by this module, using the local Naive plugin.
# Pin xcaddy so its own dependency changes cannot silently break daily syncs.
go install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.7
caddy_version=$(go list -mod=readonly -m -f '{{.Version}}' github.com/caddyserver/caddy/v2)
"$(go env GOPATH)/bin/xcaddy" build "$caddy_version" \
  --with "github.com/caddyserver/forwardproxy=$PWD"
./caddy list-modules | grep -Fx 'http.handlers.forward_proxy'
