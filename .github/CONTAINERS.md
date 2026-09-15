# Container publishing and retention

`release.yml` publishes `ghcr.io/jmylchreest/lobslaw` for every push to main,
version tag, or manual dispatch. Builds get a `sha-<short commit>` tag;
version pushes also get their release tag. There is no floating `latest` tag.
The image targets the homelab's `linux/amd64` nodes.

The weekly `prune-images.yml` workflow retains:

- All release tags and other tags that do not match `sha-<commit>`.
- The ten newest SHA builds, and everything updated in the last 30 days.
- Deployment tags listed in `PINNED_TAGS` in `scripts/prune-images.py`.
- Every child image and attestation manifest reachable from retained versions.

Older SHA builds and unreachable untagged manifests are deleted. Registry
inspection must complete successfully before any deletion starts. Add a new
deployment tag to `PINNED_TAGS` before upgrading a cluster; remove an old pin
only when no deployment or required rollback uses it. The initial homelab pin
is `sha-9dd09ba`.

The workflow token needs admin access to the package to delete versions, as
described in [GitHub's package deletion documentation](https://docs.github.com/en/packages/learn-github-packages/deleting-and-restoring-a-package).
Publishing uses the repository token; no additional publishing PAT is needed.
Cluster pulls require public package visibility or an image pull secret.

Run retention checks with:

```sh
python3 -m unittest discover -s .github/scripts -p 'test_*.py'
```
