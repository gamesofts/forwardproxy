#!/usr/bin/env bash
set -euo pipefail

# Keep stock Caddy and its QUIC implementation. Only backport the
# authenticated CONNECT read-idle fix when that patch applies.
go install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.7
caddy_version=${CADDY_VERSION:-$(go list -mod=readonly -m -f '{{.Version}}' github.com/caddyserver/caddy/v2)}
if [[ ! "$caddy_version" =~ ^v2\.[0-9]+\.[0-9]+$ ]]; then
  echo "Expected stable Caddy v2, got: $caddy_version" >&2
  exit 1
fi

repo_root=$PWD
plugins=(--with "github.com/caddyserver/forwardproxy=$repo_root")
if [[ -n "${CLOUDFLARE_VERSION:-}" ]]; then
  plugins+=(--with "github.com/caddy-dns/cloudflare@$CLOUDFLARE_VERSION")
fi

# The module's baseline Caddy version may predate this upstream fix;
# do not require a backport for builds of older Caddy releases.
extra_args=()
if [[ "$caddy_version" != "$(go list -mod=readonly -m -f '{{.Version}}' github.com/caddyserver/caddy/v2)" || "${APPLY_IDLE_PATCH:-}" == "1" ]]; then
  caddy_src=$(mktemp -d)
  trap 'rm -rf "$caddy_src"' EXIT
  git -C "$caddy_src" init -q
  git -C "$caddy_src" remote add origin https://github.com/caddyserver/caddy.git
  git -C "$caddy_src" fetch --quiet --depth=1 origin "refs/tags/$caddy_version"
  git -C "$caddy_src" checkout --quiet --detach FETCH_HEAD

  if grep -q 'func (r \*IdleTimeoutReader) Override' "$caddy_src/modules/caddyhttp/idletimeout.go" 2>/dev/null; then
    echo "Caddy already has IdleTimeoutReader.Override; skipping backport"
  elif git -C "$caddy_src" apply --check "$repo_root/caddy-read-idle-timeout.patch"; then
    git -C "$caddy_src" apply "$repo_root/caddy-read-idle-timeout.patch"
    echo "Applied read-idle fix for authenticated CONNECT streams"
  else
    echo "Caddy $caddy_version needs idle-timeout patch adaptation; refusing to silently omit the fix" >&2
    exit 1
  fi
  extra_args=(--replace "github.com/caddyserver/caddy/v2=$caddy_src")
fi

"$(go env GOPATH)/bin/xcaddy" build "$caddy_version" "${extra_args[@]}" "${plugins[@]}"
./caddy list-modules | grep -Fx 'http.handlers.forward_proxy'
if [[ -n "${CLOUDFLARE_VERSION:-}" ]]; then
  ./caddy list-modules | grep -Fx 'dns.providers.cloudflare'
fi
[[ "$(./caddy version)" == "$caddy_version "* ]]
