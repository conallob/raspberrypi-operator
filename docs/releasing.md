# Releasing

A release is a git tag. Pushing `vX.Y.Z` runs two workflows:

| Workflow | Publishes |
|---|---|
| **Release** (`release.yml`, GoReleaser) | GitHub release with `raspberrypi-operator_X.Y.Z_linux_arm64.tar.gz` (manager and agent binaries, `LICENSE`, README, `config/`, `docs/`), `install.yaml` and `checksums.txt`, plus a changelog from the commit history |
| **Image** (`image.yml`) | `ghcr.io/conallob/raspberrypi-operator:X.Y.Z` (arm64) |
| **Release**, last step | the Helm chart as an OCI chart, `oci://ghcr.io/conallob/charts/raspberrypi-operator:X.Y.Z` (also attached to the release as a `.tgz`) |

`install.yaml` is `config/default` with the image pinned to `:X.Y.Z`, so it
matches the image the Image workflow pushes for the same tag. The image can
land a few minutes after the release; the release notes say so.

Everything is arm64-only, as every supported board is.

## Cutting a release

```bash
make test lint
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

A tag with a suffix (`v0.1.0-rc.1`) is published as a pre-release.

## Trying it first

CI runs the whole release except the publish on every pull request
(`release-snapshot`). Locally:

```bash
make release-check      # validate .goreleaser.yaml
make release-snapshot   # build everything into dist/ without tagging or publishing
```

Snapshot builds are versioned `<next patch>-SNAPSHOT-<commit>`.

## Version stamping

The binaries report their version, commit and date, set at link time into
`internal/version`:

```bash
agent -version     # raspberrypi-operator agent 0.1.0 (commit abc1234, built 2026-11-14T...)
```

The same three variables are set by GoReleaser (`ldflags` in `.goreleaser.yaml`),
by `make build` (from `git describe`), and by the Dockerfile (`VERSION`,
`COMMIT`, `DATE` build args, passed by `image.yml`), so a binary, a release
archive and a container image all agree. A plain `go build` reports `dev`.
Both processes also log the version at startup.

## The Helm chart

The chart in `charts/raspberrypi-operator` is packaged at the release version
(chart `version` and `appVersion` both `X.Y.Z`), so its default image tag is the
image the Image workflow pushes for the same tag. `make chart-package` and
`make chart-push` do the packaging and the OCI push. CRDs and the manager's
RBAC in the chart are generated from `config/` (`make chart-sync`); CI fails if
they drift.

## Not done yet

- **Signing and SBOMs.** Release assets are covered by `checksums.txt` only;
  there is no cosign signature or SBOM.
- **Image provenance.** The image is built by a separate workflow, not by
  GoReleaser, so the two share a tag but not a build.
- **Pinning.** GoReleaser is pinned to v2.20.2 in both the Makefile and
  `release.yml`; bump them together. `hack/release-manifests.sh` pins kustomize
  and the Makefile pins helm.
- **Chart publishing is untested.** The OCI push needs a real registry; the
  first tag (use a `-rc`) is its first run. Packaging is exercised by the
  snapshot release.
