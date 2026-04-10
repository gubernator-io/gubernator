# Release Process

## How to Release

1. Merge all PRs intended for this release to `master`.
2. Create a new [GitHub Release](https://github.com/gubernator-io/gubernator/releases/new) with a tag in the format `vMAJOR.MINOR.PATCH` (e.g., `v2.17.0`).
3. Click **Publish Release**.

The `on-release` workflow handles everything else:
- Updates the Helm chart version from the release tag.
- Builds and pushes the Docker image for `linux/amd64` and `linux/arm64`.
- Packages and pushes the Helm chart to GHCR as an OCI artifact (`oci://ghcr.io/gubernator-io/charts/gubernator`).

## Version Derivation

There is no `version` file to maintain. Versions are derived from git tags:
- **Release builds** (CI): use `github.event.release.tag_name` directly.
- **Local builds** (`make build`): use `git describe --tags --always --dirty`, producing versions like `v2.17.0` on a tagged commit or `v2.17.0-5-gabcdef-dirty` during development.
