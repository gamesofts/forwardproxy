# Upstream synchronization

This fork preserves the Naive padding protocol from `klzgrad/forwardproxy:naive`
and merges changes directly from `caddyserver/forwardproxy:master`.

The **Sync Caddy upstream** workflow checks daily at 03:23 UTC, or on demand
using **Actions → Sync Caddy upstream → Run workflow**. It merges upstream into
`sync/caddy-master`, runs the race-enabled test suite, builds the Caddy version
declared in `go.mod` with the local plugin, and creates or updates one PR against
the default branch. It never automatically merges a PR or force-pushes a branch.

Use **Create a merge commit** when merging sync PRs. Squashing or rebasing loses
the upstream ancestry used to detect which changes are already synchronized.

## Initial setup

After merging the setup PR, enable Actions if GitHub has disabled workflows for
this fork. In **Settings → Actions → General → Workflow permissions**, enable
**Allow GitHub Actions to create and approve pull requests**. The workflow needs
to create PRs; it does not approve them. No personal access token is required.
The workflow explicitly requests contents and pull-request write permissions.

GitHub does not start other workflows for a PR created with `GITHUB_TOKEN`, so
the sync workflow itself runs tests and the build before pushing and includes a
link to that successful run in the PR. Manually created PRs additionally run the
regular Linux/Windows tests and the Caddy build.

## Conflicts or validation failures

A conflict or failed test/build stops the workflow before pushing. The default
branch remains untouched. The Actions log and summary show the failed step.
Resolve conflicts on `sync/caddy-master` and rerun the workflow; an existing
branch is retained, including any manual resolution commits. Do not add unrelated
work to this dedicated branch.

GitHub can suspend scheduled workflows in public repositories after 60 days of
repository inactivity. If that happens, re-enable the workflow in Actions.

## Automatic Caddy releases

**Release Caddy with Naive and Cloudflare** checks the latest official Caddy stable release
daily at 04:37 UTC. After this workflow is merged into `naive`, its first
push run also builds the current latest version if it has not been published.
Use **Run workflow** with an optional stable tag such as `v2.11.7` to build a
specific release. Drafts and prereleases are rejected.

Only **Linux amd64** is built, with CGO disabled. Release tags use
`caddy-vX.Y.Z`; assets are `caddy-vX.Y.Z-linux-amd64.tar.gz` and `SHA256SUMS`.
The archive includes `caddy`, `LICENSE`, `README.md`, and `BUILD_INFO.json` with
the Caddy version, this fork's source commit and build-run URL.

Every release includes `github.com/caddy-dns/cloudflare`, resolved to its latest
Go module version at build time and recorded in the build metadata. Module
registration and Cloudflare Caddyfile adaptation are checked without credentials
or calls to the Cloudflare API. Supply your own token when using DNS challenges.
The Go toolchain is selected from the Caddy release's `go.mod` requirement.

The workflow tests the plugin against the selected Caddy version in a temporary
module, builds with xcaddy, verifies the binary version and module registration,
and validates a Caddyfile with authentication and probe resistance. It leaves
this repository's `go.mod` and `go.sum` unchanged. PR runs validate and upload
an Actions artifact; only trusted default-branch runs publish Releases.

Publishing uses a draft and uploads both assets before making it public.
Complete public releases are never rebuilt or overwritten. Interrupted drafts
are retried with their original source commit; a failed test/build publishes
nothing. Scheduled runs select the latest stable release visible at check time;
intermediate versions can be built with the manual version input.

No PAT or external service is needed. The publish job requests contents write
permission from `GITHUB_TOKEN`. The earlier **Build** workflow now only produces
CI artifacts, so it cannot attach an older Caddy build to these new releases.
