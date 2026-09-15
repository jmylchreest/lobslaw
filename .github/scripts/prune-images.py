#!/usr/bin/env python3
"""Prune old SHA builds without deleting manifests reachable from retained tags."""

import datetime as dt
import json
import os
import re
import subprocess
from urllib.parse import quote

KEEP_BUILDS = 10
MIN_AGE_DAYS = 30
PAGE_SIZE = 100
# Deployment pins must survive even after they fall outside the build window.
PINNED_TAGS = {"sha-9dd09ba", "sha-6d29f6b"}
SHA_TAG = re.compile(r"sha-[0-9a-f]{7,40}")


def command(*args):
    return subprocess.check_output(args, text=True)


def candidates(versions, now):
    cutoff = now - dt.timedelta(days=MIN_AGE_DAYS)
    builds = sorted(
        (v for v in versions if v["metadata"]["container"]["tags"]
         and all(SHA_TAG.fullmatch(t) for t in v["metadata"]["container"]["tags"])),
        key=lambda v: v["created_at"], reverse=True,
    )
    protected = {v["id"] for v in builds[:KEEP_BUILDS]}
    return [v for v in versions
            if v["id"] not in protected
            and dt.datetime.fromisoformat(v["updated_at"].replace("Z", "+00:00")) < cutoff
            and not PINNED_TAGS.intersection(v["metadata"]["container"]["tags"])
            and all(SHA_TAG.fullmatch(t) for t in v["metadata"]["container"]["tags"])]


def reachable(roots, inspect):
    seen = set()
    pending = list(roots)
    while pending:
        digest = pending.pop()
        if digest in seen:
            continue
        seen.add(digest)
        manifest = inspect(digest)
        pending.extend(m["digest"] for m in manifest.get("manifests", []))
    return seen


def main():
    repository = os.environ["GITHUB_REPOSITORY"].lower()
    owner, package = repository.split("/", 1)
    account = json.loads(command("gh", "api", f"users/{owner}"))
    scope = "orgs" if account["type"] == "Organization" else "users"
    endpoint = f"{scope}/{owner}/packages/container/{quote(package, safe='')}/versions"
    pages = json.loads(command("gh", "api", "--paginate", "--slurp",
                               f"{endpoint}?per_page={PAGE_SIZE}"))
    versions = [v for page in pages for v in page]
    deletable = candidates(versions, dt.datetime.now(dt.timezone.utc))
    ids = {v["id"] for v in deletable}
    # Inspect all retained roots before deleting anything. An API/registry
    # failure aborts cleanup rather than treating missing evidence as garbage.
    roots = [v["name"] for v in versions if v["id"] not in ids]
    live = reachable(roots, lambda digest: json.loads(command(
        "docker", "buildx", "imagetools", "inspect", "--raw",
        f"ghcr.io/{repository}@{digest}")))
    for version in deletable:
        if version["name"] in live:
            continue
        print(f"Deleting version {version['id']} {version['name']}", flush=True)
        subprocess.run(["gh", "api", "--method", "DELETE",
                        f"{endpoint}/{version['id']}"], check=True)


if __name__ == "__main__":
    main()
