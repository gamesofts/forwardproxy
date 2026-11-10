# Secure forward proxy for the Caddy web server

This package registers the `http.handlers.forward_proxy` module, which acts as an HTTPS proxy for accessing remote networks.

## :warning: Experimental!

This module is EXPERIMENTAL. We need more users to test this module for bugs and weaknesses before we recommend its use from within surveilled networks or regions with active censorship. Do not rely on this code in situations where personal safety, freedom, or privacy are at risk.

**You can help by:**

- Safely deploying this module
- Trying to break it
- Contributing to the code and tests in this repo to make it better

We are also seeking experienced maintainers who have experience with these kinds of technologies and who are interested in continuing its development.

**Expect breaking changes.**

## Features

- HTTP/1.1, HTTP/2, and HTTP/3 support
- Authentication
- Optional probe resistance
- PAC file
- Naive-compatible SagerNet UDP over TCP (UoT v1 and v2)

## UDP over TCP with Naive

This fork accepts SagerNet's UoT protocol inside a normal CONNECT tunnel,
including Naive padding. It recognizes `sp.udp-over-tcp.arpa` (v1) and
`sp.v2.udp-over-tcp.arpa` (v2); these are protocol markers, not DNS names.
The integration is adapted from [SagerNet/forwardproxy](https://github.com/SagerNet/forwardproxy)
and [aUsernameWoW/forwardproxy:udpintcp](https://github.com/aUsernameWoW/forwardproxy/tree/5cf089c17a07adc32fb178957ee8a6758092cccf),
using the [sing UoT codec](https://github.com/SagerNet/sing/tree/v0.7.5/common/uot).

Build this fork, rather than the original upstream module:

```sh
xcaddy build --with github.com/caddyserver/forwardproxy=github.com/gamesofts/forwardproxy@naive --with github.com/caddy-dns/cloudflare
```

No additional `forward_proxy` Caddyfile option is needed. Enable UoT in a
supporting client, for example in the sing-box `naive` outbound:

```json
"udp_over_tcp": {
  "enabled": true,
  "version": 2
}
```

This is the SagerNet extension used by sing-box, not MASQUE CONNECT-UDP or
native UDP support for every standalone NaiveProxy client. With HTTP/2, UDP
payloads travel inside the existing TCP/TLS connection; the server needs
outbound UDP connectivity to the requested destinations, with no extra inbound
UDP listener for UoT. HTTP/3 uses QUIC as its outer transport instead.

Real UDP destinations use the `ports` restriction but no IP/domain ACL.
The magic address's port (normally 0) is not the target UDP port.
Upstream proxies receive the original magic CONNECT request and must support
UoT at the final hop. Tunnels close on malformed input, rejected ports,
fixed-target I/O errors, client disconnect, or two minutes without traffic.
In multi-target mode, blocked-port/unresolvable destinations are dropped without
interrupting other traffic. One UDP socket preserves the local source port
across targets; a bounded 256-entry DNS/destination cache expires entries after
one minute. Fixed-target v2 mode uses a connected UDP socket. Empty UDP datagrams,
65,000-byte payloads and both v2 connect/non-connect framing are tested.


## Introduction

This Caddy module allows you to use your web server as a proxy server, configurable by numerous HTTP clients such as operating systems, web browsers, mobile devices, and apps. However, the feature set of each client varies widely, as does their correctness and security guarantees. You will have to be aware of each clients' individual weaknesses or shortcomings.


## Quick start

First, you will have to know [how to use Caddy](https://caddyserver.com/docs/getting-started).

Build Caddy with this plugin. You can add it from [Caddy's download page](https://caddyserver.com/download) or build it yourself with [xcaddy](https://github.com/caddyserver/xcaddy):

```
$ xcaddy build --with github.com/caddyserver/forwardproxy
```

Most people prefer the [Caddyfile](https://caddyserver.com/docs/caddyfile) for configuration. The following unauthenticated example is **only for isolated tests**, never a public VPS:

```
:443, example.com {
	# UNAUTHENTICATED! USE ONLY FOR TESTING
	forward_proxy
}
```

(Obviously, replace `example.com` with your domain name which is pointed at your machine.)

The `forward_proxy` has a default [directive order](https://caddyserver.com/docs/caddyfile/directives#directive-order) of "after `file_server`". You may put it inside a `route` block or use the `order` global option to override this. The correct order is up to you and depends on your config. For example:

```
{
	order forward_proxy first
}

:443, example.com {
	# UNAUTHENTICATED! USE ONLY FOR TESTING
	forward_proxy
}
```

This plugin enables [Caddy](https://caddyserver.com) to act as a forward proxy, with support for HTTP/3, HTTP/2, and HTTP/1.1 requests. HTTP/3 and HTTP/2 will usually improve performance due to multiplexing.

Forward proxy plugin includes authentication, as well as some unique features to assist with security and privacy. Default configuration of forward proxy is compliant with existing HTTP standards, but some features force plugin to exhibit non-standard but non-breaking behavior to preserve privacy.

Probing resistance—one of the signature features of this plugin—attempts to hide the fact that your webserver is also a forward proxy, helping the proxy to stay under the radar. Eventually, forwardproxy plugin implemented a simple *reverse* proxy (`upstream https://user:password@next-hop.com` in Caddyfile) just so users may take advantage of `probe_resistance` when they need a reverse proxy (for example, to build a chain of proxies). Reverse proxy implementation will stay simple, and if you need a powerful reverse proxy, look into Caddy's standard `proxy` directive.

For a complete list of features and their usage, see Caddyfile syntax:

## Caddyfile Syntax (Server Configuration)

The simplest way to enable the forward proxy without authentication just include the `forward_proxy` directive in your Caddyfile. However, this allows anyone to use your server as a proxy, which might not be desirable.

In the Caddyfile the addresses must start with `:443` for the `forward_proxy` to work for proxy requests of all origins.

Here's an example of all properties in use (note that the syntax is subject to change):

```
forward_proxy {
	basic_auth user1 0NtCL2JPJBgPPMmlPcJ
	basic_auth user2 密码

	ports     80 443
	hide_ip
	hide_via
	disable_insecure_upstreams_check
	probe_resistance secret-link-kWWL9Q.com # alternatively you can use a real domain, such as caddyserver.com
	serve_pac /secret-proxy.pac

	dial_timeout            30s

	max_idle_conns          50
	max_idle_conns_per_host 2

	upstream https://user:password@extra-upstream-hop.com

}
```

(The square brackets `[ ]` indicate values you should replace; do not actually include the brackets.)

### Security

- `basic_auth [user] [password]`  
  Sets basic HTTP auth credentials. This property may be repeated multiple times. Note that this is different from Caddy's built-in `basic_auth` directive. BE SURE TO CHECK THE NAME OF THE SITE THAT IS REQUESTING CREDENTIALS BEFORE YOU ENTER THEM.

  Default: no authentication required.
- `probe_resistance [secretlink.tld]`  
  Attempts to hide the fact that the site is a forward proxy.
  Proxy will no longer respond with "407 Proxy Authentication Required" if credentials are incorrect or absent,
  and will attempt to mimic a generic Caddy web server as if the forward proxy is not enabled.

  Probing resistance works (and makes sense) only if `basic_auth` is set up.
  To use your proxy with probe resistance, supply your `basic_auth` credentials to your client configuration.
  If your proxy client(browser, operating system, browser extension, etc)
  allows you to preconfigure credentials, and sends credentials preemptively, you do not need secret link.

  If your proxy client does not preemptively send credentials, you will have to visit your secret link in your browser to trigger the authentication.
  Make sure that specified domain name is visitable, does not contain uppercase characters, does not start with dot, etc.
  Only this address will trigger a 407 response, prompting browsers to request credentials from user and cache them for the rest of the session.

  Default: no probing resistance.
- `disable_insecure_upstreams_check`  
  Disables the check for insecure (HTTP) upstreams. By default, forwardproxy will refuse to connect to upstreams that are not using TLS. This option disables that check.

  Default: check for insecure upstreams.

### Privacy

- `hide_ip`  
  If set, forwardproxy will not add user's IP to "Forwarded:" header.
  
  WARNING: there are other side-channels in your browser, that you might want to eliminate, such as WebRTC, see [here](https://www.ivpn.net/knowledgebase/158/My-IP-is-being-leaked-by-WebRTC-How-do-I-disable-it.html) how to disable it.
  
  Default: no hiding; `Forwarded: for="useraddress"` will be sent out.
- `hide_via`  
  If set, forwardproxy will not add Via header, and prevents simple way to detect proxy usage.

  WARNING: there are other side-channels to determine this.

  Default: no hiding; Header in form of `Via: 2.0 caddy` will be sent out.

### Access Control

- `ports [integer] [integer]...`  
  Specifies ports forwardproxy will whitelist for all requests. Other ports will be forbidden.

  Default: no restrictions.
**Security note for private VPS deployments:** Destination IP/domain ACLs have
been removed in this fork. TCP destinations are resolved and connected using
Go's native dialer, including IPv4/IPv6 Happy Eyeballs. Domains resolving to
loopback, private, link-local or cloud metadata IPs are **not blocked**.
Only run this configuration with `basic_auth` and restrict access at the VPS
firewall/security group; an unauthenticated public proxy is unsafe.
Old `acl { ... }` blocks must be removed from Caddyfile/JSON configuration.
The `ports` restriction remains available.

### Timeouts

- `dial_timeout [Duration]`  
  Sets timeout (with units, e.g. 30s) for establishing TCP connection to target website. Affects all requests.

  Default: 30 seconds.

### Pooling

By default, forwardproxy will reuse connections by using Go's built-in connection pooling mechanism. You can adjust the maximum number of idle connections to keep open:

- `max_idle_conns [integer]`
  Sets the maximum number of idle connections to keep open, globally. Set to -1 for no global limit. See https://pkg.go.dev/net/http#Transport.MaxIdleConns
  Default: 50.

- `max_idle_conns_per_host [integer]`
  Sets the maximum number of idle connections to keep open _per host_. See https://pkg.go.dev/net/http#Transport.MaxIdleConnsPerHost
  Default: 0, which uses Go's default of 2.

### Other

- `serve_pac [/path.pac]`  
  Generate (in-memory) and serve a [Proxy Auto-Config](https://en.wikipedia.org/wiki/Proxy_auto-config) file on given path. If no path is provided, the PAC file will be served at `/proxy.pac`. NOTE: If you enable probe_resistance, your PAC file should also be served at a secret location; serving it at a predictable path can easily defeat probe resistance.

  Default: no PAC file will be generated or served by Caddy (you still can manually create and serve proxy.pac like a regular file).
- `upstream [https://username:password@upstreamproxy.site:443]`  
  Sets upstream proxy to route all forwardproxy requests through it.
  This setting does not affect non-forwardproxy requests nor requests with wrong credentials.
  For upstream connections, the upstream proxy owns destination policy, including port restrictions.

  Supported schemes to remote host: https.

  Supported schemes to localhost: socks5, http, https (certificate check is ignored).

  Default: no upstream proxy.

## Get forwardproxy
### Download prebuilt binary
Binaries are at https://caddyserver.com/download  
Don't forget to add `http.forwardproxy` plugin.

### Build from source

0. Install latest Golang 1.20 or above and set `export GO111MODULE=on`
1. `go install github.com/caddyserver/forwardproxy/cmd/caddy@latest`  
   Built `caddy` binary will be stored in $GOPATH/bin.  

## Client Configuration

Please be aware that client support varies widely, and there are edge cases where clients may not use the proxy when it should or could. It's up to you to be aware of these limitations.

The basic configuration is simply to use your site address and port (usually for all protocols - HTTP, HTTPS, etc). You can also specify the .pac file if you enabled that.

Read [this blog post](https://sfrolov.io/2017/08/secure-web-proxy-client-en) about how to configure your specific client.

## License

Licensed under the [Apache License](LICENSE)

## Disclaimers

USE AT YOUR OWN RISK. THIS IS DELIVERED AS-IS. By using this software, you agree and assert that authors, maintainers, and contributors of this software are not responsible or liable for any risks, costs, or problems you may encounter. Consider your threat model and be smart. If you find a flaw or bug, please submit a patch and help make things better!

Initial version of this plugin was developed by Google. This is not an official Google product.
