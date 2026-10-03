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
