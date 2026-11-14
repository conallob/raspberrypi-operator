#!/bin/sh
# Render the install manifests for a release: config/default with the image
# pinned to VERSION instead of :latest.
#
#   hack/release-manifests.sh 1.2.3 dist/install.yaml
#
# Works on a copy, so the checked-in kustomization is never modified.
set -eu

version=${1:?usage: release-manifests.sh VERSION OUTFILE}
out=${2:?usage: release-manifests.sh VERSION OUTFILE}
image=ghcr.io/conallob/raspberrypi-operator
kustomize="go run sigs.k8s.io/kustomize/kustomize/v5@v5.6.0"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -R config "$work/config"

(cd "$work/config/default" && $kustomize edit set image "$image=$image:$version")
mkdir -p "$(dirname "$out")"
$kustomize build "$work/config/default" > "$out"

# Refuse to ship a manifest that still points at a floating tag.
if grep -q "image: $image:latest" "$out"; then
	echo "release-manifests: $out still references $image:latest" >&2
	exit 1
fi
grep -c "image: $image:$version" "$out" > /dev/null || { echo "release-manifests: no image pinned to $version" >&2; exit 1; }
