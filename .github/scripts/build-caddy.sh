#!/usr/bin/env bash
set -euo pipefail

# Build the Caddy version tested by this module, using the local Naive plugin.
# Pin xcaddy so its own dependency changes cannot silently break daily syncs.
go install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.7
caddy_module=github.com/caddyserver/caddy/v2
caddy_version=${CADDY_VERSION:-$(go list -mod=readonly -m -f '{{.Version}}' "$caddy_module")}
if [[ ! "$caddy_version" =~ ^v2\.[0-9]+\.[0-9]+$ ]]; then
  echo "Expected a stable Caddy v2 release, got: $caddy_version" >&2
  exit 1
fi

# Give streams room to share the connection when one upload stalls.
go mod download "$caddy_module@$caddy_version"
caddy_source=$(mktemp -d)
trap 'rm -rf "$caddy_source"' EXIT
cp -R "$(go env GOMODCACHE)/$caddy_module@$caddy_version/." "$caddy_source/"
chmod -R u+w "$caddy_source"
caddy_app="$caddy_source/modules/caddyhttp/app.go"
if ! awk '
  /^[[:space:]]*h2server := new\(http2\.Server\)$/ ||
  /^[[:space:]]*http2\.ConfigureServer\(srv\.server, new\(http2\.Server\)\)$/ {
    sub(/new\(http2\.Server\)/, "\\&http2.Server{MaxUploadBufferPerConnection: 4 << 20, MaxUploadBufferPerStream: 1 << 20}")
    matches++
  }
  { print }
  END { if (matches != 1) exit 1 }
' "$caddy_app" > "$caddy_app.patched"; then
  echo "Cannot apply the HTTP/2 receive windows to Caddy $caddy_version" >&2
  exit 1
fi
mv "$caddy_app.patched" "$caddy_app"

plugins=(--with "github.com/caddyserver/forwardproxy=$PWD")
if [[ -n "${CLOUDFLARE_VERSION:-}" ]]; then
  plugins+=(--with "github.com/caddy-dns/cloudflare@$CLOUDFLARE_VERSION")
fi
"$(go env GOPATH)/bin/xcaddy" build "$caddy_version" --replace "$caddy_module=$caddy_source" "${plugins[@]}"
./caddy list-modules | grep -Fx 'http.handlers.forward_proxy'
if [[ -n "${CLOUDFLARE_VERSION:-}" ]]; then
  ./caddy list-modules | grep -Fx 'dns.providers.cloudflare'
fi
[[ "$(./caddy version)" == "$caddy_version "* ]]
