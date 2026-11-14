#!/bin/sh
# Render the chart in the configurations internal/charttest checks, into OUTDIR.
# Needs HELM (a helm 3 command line) in the environment; see the Makefile.
set -eu

out=${1:?usage: render-chart.sh OUTDIR}
helm=${HELM:?set HELM to a helm 3 command line}
chart=charts/raspberrypi-operator
kube="--kube-version 1.31.0" # helm template otherwise assumes Kubernetes 1.20

mkdir -p "$out"
render() { name=$1; shift; $helm template "$@" $kube > "$out/$name.yaml"; }

render default       rel "$chart" -n pis-system
render policy        rel "$chart" -n pis-system -f "$chart/ci/policy-values.yaml"
render renamed       pis "$chart" -n other-ns --set fullnameOverride=pi-op
render minimal       rel "$chart" -n pis-system --set crds.install=false --set admissionPolicy.enabled=false
render custom-image  rel "$chart" -n pis-system --set image.repository=registry.example/pi --set image.tag=1.2.3

# values.schema.json must reject mistakes at install time.
: > "$out/schema-rejections.txt"
reject() {
	name=$1; shift
	if $helm template rel "$chart" -n pis-system $kube "$@" > /dev/null 2>&1; then
		echo "ACCEPTED $name" >> "$out/schema-rejections.txt"
	else
		echo "rejected $name" >> "$out/schema-rejections.txt"
	fi
}
reject zero-replicas        --set manager.replicas=0
reject string-replicas      --set manager.replicas=many
reject bad-pull-policy      --set image.pullPolicy=Sometimes
reject bad-interval         --set agent.interval=soon
reject relative-bootfs      --set agent.bootFS.hostPath=boot/firmware
reject bad-validation       --set 'admissionPolicy.validationActions={Block}'
reject typo-top-level       --set managr.replicas=2
reject typo-nested          --set agent.bootFs.hostPath=/boot
reject policy-without-spec  --set bootloaderPolicy.create=true --set bootloaderPolicy.name=x
reject policy-without-name  --set bootloaderPolicy.create=true --set-json 'bootloaderPolicy.spec={"a":1}'
