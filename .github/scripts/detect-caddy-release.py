#!/usr/bin/env python3
"""Select a stable Caddy release and detect complete/draft fork releases."""

import json
import base64
import os
import re
import subprocess


def gh_api(path):
    result = subprocess.run(
        ["gh", "api", path], capture_output=True, text=True, check=False
    )
    if result.returncode:
        if "HTTP 404" in result.stderr:
            return None
        raise RuntimeError(f"GitHub API request failed: {result.stderr.strip()}")
    return json.loads(result.stdout)


def select_release(upstream, existing):
    if upstream is None:
        raise ValueError("Caddy release does not exist")
    version = upstream["tag_name"]
    if not re.fullmatch(r"v2\.[0-9]+\.[0-9]+", version):
        raise ValueError(f"Not a stable Caddy v2 tag: {version}")
    if upstream.get("draft") or upstream.get("prerelease"):
        raise ValueError("Draft and prerelease Caddy versions are not published")
    tag = f"caddy-{version}"
    archive = f"caddy-{version}-linux-amd64.tar.gz"
    complete = False
    if existing is not None and not existing.get("draft"):
        assets = {asset["name"] for asset in existing.get("assets", [])}
        if not {archive, "SHA256SUMS"}.issubset(assets):
            raise ValueError("Published fork release is incomplete; repair it manually")
        complete = True
    return {
        "version": version,
        "tag": tag,
        "archive": archive,
        "should_build": str(not complete).lower(),
        "release_exists": str(existing is not None).lower(),
    }


def main():
    requested = os.environ.get("REQUESTED_VERSION", "").strip()
    if requested and not re.fullmatch(r"v2\.[0-9]+\.[0-9]+", requested):
        raise ValueError("Version must look like v2.11.7, without prerelease suffixes")
    endpoint = f"tags/{requested}" if requested else "latest"
    upstream = gh_api(f"repos/caddyserver/caddy/releases/{endpoint}")
    # Validate before inserting an upstream tag into a REST path.
    selection = select_release(upstream, None)
    repository = os.environ["GITHUB_REPOSITORY"]
    existing = gh_api(f"repos/{repository}/releases/tags/{selection['tag']}")
    selection = select_release(upstream, existing)
    source_sha = os.environ["SOURCE_SHA"]
    # Retry a partial draft using the same source snapshot as its first build.
    if existing is not None and existing.get("draft") and os.environ.get("GITHUB_EVENT_NAME") != "pull_request":
        source_sha = existing.get("target_commitish", "")
    if not re.fullmatch(r"[0-9a-f]{40}", source_sha):
        raise ValueError("Release source must be an immutable commit SHA")
    selection["source_sha"] = source_sha
    latest = gh_api("repos/caddyserver/caddy/releases/latest") if requested else upstream
    selection["make_latest"] = str(latest is not None and latest["tag_name"] == selection["version"]).lower()
    module = gh_api(f"repos/caddyserver/caddy/contents/go.mod?ref={selection['version']}")
    if module is None:
        raise ValueError("Caddy go.mod is unavailable")
    go_mod = base64.b64decode(module["content"]).decode("utf-8")
    required_go = re.search(r"^go (\d+\.\d+)(?:\.\d+)?$", go_mod, re.MULTILINE)
    if required_go is None:
        raise ValueError("Caddy Go version requirement is missing")
    selection["go_version"] = required_go[1] + ".x"
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        for key, value in selection.items():
            output.write(f"{key}={value}\n")
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as summary:
        summary.write(f"Caddy {selection['version']}: build required = {selection['should_build']}.\n")


if __name__ == "__main__":
    main()
