#!/usr/bin/env bash
set -euo pipefail

# Build Caddy with the local Naive forwardproxy plugin and the klzgrad
# Caddy patches required for sing-quic BBRv1 and authenticated-stream
# idle-timeout handling. A patch failure is fatal so a future Caddy
# release cannot silently lose either behavior.
go install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.7

if [[ -n "${CADDY_VERSION:-}" ]]; then
  caddy_version=$CADDY_VERSION
elif [[ -f CADDY_VERSION ]]; then
  caddy_version=$(<CADDY_VERSION)
else
  caddy_version=$(go list -mod=readonly -m -f '{{.Version}}' github.com/caddyserver/caddy/v2)
fi
if [[ ! "$caddy_version" =~ ^v2\.[0-9]+\.[0-9]+$ ]]; then
  echo "Expected a stable Caddy v2 release, got: $caddy_version" >&2
  exit 1
fi

repo_root=$PWD
caddy_src=$(mktemp -d)
trap 'rm -rf "$caddy_src"' EXIT

git -C "$caddy_src" init -q
git -C "$caddy_src" remote add origin https://github.com/caddyserver/caddy.git
git -C "$caddy_src" fetch --quiet --depth=1 origin "refs/tags/$caddy_version"
git -C "$caddy_src" checkout --quiet --detach FETCH_HEAD
git -C "$caddy_src" apply   "$repo_root/caddy-sing-quic-bbrv1.patch"   "$repo_root/caddy-read-idle-timeout.patch"

# Guard the two upstream behaviors before spending time compiling.
grep -F 'SetCongestionControl(bbr.NewBbrSenderWithProfile' "$caddy_src/modules/caddyhttp/server.go" >/dev/null
grep -F 'func (r *IdleTimeoutReader) Override' "$caddy_src/modules/caddyhttp/idletimeout.go" >/dev/null

plugins=(--with "github.com/caddyserver/forwardproxy=$repo_root")
if [[ -n "${CLOUDFLARE_VERSION:-}" ]]; then
  plugins+=(--with "github.com/caddy-dns/cloudflare@$CLOUDFLARE_VERSION")
fi

"$(go env GOPATH)/bin/xcaddy" build "$caddy_version"   --replace "github.com/caddyserver/caddy/v2=$caddy_src"   "${plugins[@]}"

./caddy list-modules | grep -Fx 'http.handlers.forward_proxy'
if [[ -n "${CLOUDFLARE_VERSION:-}" ]]; then
  ./caddy list-modules | grep -Fx 'dns.providers.cloudflare'
fi
go version -m ./caddy | grep -F 'github.com/sagernet/sing-quic' >/dev/null
[[ "$(./caddy version)" == "$caddy_version "* ]]
