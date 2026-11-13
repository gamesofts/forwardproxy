#!/usr/bin/env bash
set -euo pipefail

# Build the Caddy version tested by this module, using the local Naive plugin.
# Pin xcaddy so its own dependency changes cannot silently break daily syncs.
go install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.7
caddy_version=${CADDY_VERSION:-$(go list -mod=readonly -m -f '{{.Version}}' github.com/caddyserver/caddy/v2)}
if [[ ! "$caddy_version" =~ ^v2\.[0-9]+\.[0-9]+$ ]]; then
  echo "Expected a stable Caddy v2 release, got: $caddy_version" >&2
  exit 1
fi
plugins=(--with "github.com/caddyserver/forwardproxy=$PWD")
if [[ -n "${CLOUDFLARE_VERSION:-}" ]]; then
  plugins+=(--with "github.com/caddy-dns/cloudflare@$CLOUDFLARE_VERSION")
fi
"$(go env GOPATH)/bin/xcaddy" build "$caddy_version" "${plugins[@]}"
./caddy list-modules | grep -Fx 'http.handlers.forward_proxy'
if [[ -n "${CLOUDFLARE_VERSION:-}" ]]; then
  ./caddy list-modules | grep -Fx 'dns.providers.cloudflare'
fi
[[ "$(./caddy version)" == "$caddy_version "* ]]
