# raspberrypi-operator Helm chart

Installs the [raspberrypi-operator](https://github.com/conallob/raspberrypi-operator):
the manager (Deployment), the per-node agent (DaemonSet), the CRDs, RBAC and an
admission policy. It is equivalent to `kubectl apply -k config/default`, and a
test keeps the two in step.

> Alpha, and not yet run on real hardware. Try it on a spare node first.

## Install

```bash
helm install raspberrypi-operator \
  oci://ghcr.io/conallob/charts/raspberrypi-operator \
  --version <version> \
  --namespace raspberrypi-operator-system --create-namespace
```

From a checkout (the image tag then defaults to `latest`):

```bash
helm install raspberrypi-operator charts/raspberrypi-operator \
  --namespace raspberrypi-operator-system --create-namespace
```

Nothing touches a node until a `BootloaderPolicy` selects it. Either apply one
yourself (`config/samples/bootloaderpolicy.yaml`) or declare it in values; see
below.

## Values

Every value is type-checked by `values.schema.json`, so a typo (`managr:`), a
wrong type or an out-of-range value fails at `helm install` instead of being
ignored.

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/conallob/raspberrypi-operator` | arm64 image holding `/manager` and `/agent` |
| `image.tag` | chart `appVersion` | |
| `image.pullPolicy` | `IfNotPresent` | `Always`, `IfNotPresent` or `Never` |
| `imagePullSecrets` | `[]` | |
| `nameOverride`, `fullnameOverride` | `""` | Cluster-scoped objects are prefixed with the full name |
| `crds.install` | `true` | Install the CRDs. They are templates (upgraded with the release) annotated `helm.sh/resource-policy: keep`, so uninstalling never deletes your policies |
| `manager.replicas` | `2` | Failover only: leader election runs one |
| `manager.resources`, `.nodeSelector`, `.tolerations`, `.affinity`, `.extraArgs`, `.podAnnotations` | | `nodeSelector` defaults to `kubernetes.io/arch: arm64` |
| `agent.interval` | `30s` | How often a node re-checks its bootloader |
| `agent.bootFS.hostPath` | `/boot/firmware` | Where the host mounts its boot partition |
| `agent.resources`, `.nodeSelector`, `.tolerations`, `.priorityClassName`, `.extraArgs`, `.podAnnotations` | | Tolerates every taint by default |
| `admissionPolicy.enabled` | `true` | Restrict each agent to its own node's `NodeBootloader` (Kubernetes 1.30+) |
| `admissionPolicy.failurePolicy` | `Fail` | `Fail` or `Ignore` |
| `admissionPolicy.validationActions` | `[Deny]` | Use `[Warn, Audit]` on an older cluster |
| `bootloaderPolicy.create` | `false` | Also create a `BootloaderPolicy` from `bootloaderPolicy.spec` |
| `bootloaderPolicy.name`, `.spec` | | The policy; a complete example is `ci/policy-values.yaml` |

## Managing policies with the operator, as one unit

```yaml
# values.yaml
bootloaderPolicy:
  create: true
  name: pis
  spec:
    nodeSelector: {matchLabels: {rpi.conall.dev/managed: "true"}}
    firmware:
      images:
        - {chip: bcm2712, buildTimestamp: 1793393754, url: "https://…", sha256: "…"}
    config:
      bootOrder: [nvme, usb, sd, restart]
```

Bumping the firmware is then a values change, reviewed and rolled out like any
other. The spec is validated by the CRD when applied.

## GitOps

With Argo CD or Flux, point the application at this chart (or the OCI
repository) and put your values in Git. If you already manage the CRDs
separately (for example `kubectl apply -k config/crd`), set `crds.install=false`.

## Using it as a base

The chart prefixes every cluster-scoped name with the release name, so releases
don't collide, and it works as a dependency of your own chart:

```yaml
# Chart.yaml of your chart
dependencies:
  - name: raspberrypi-operator
    version: <version>
    repository: oci://ghcr.io/conallob/charts
```

```yaml
# values.yaml of your chart: everything above goes under the dependency's name
raspberrypi-operator:
  agent:
    interval: 1m
  bootloaderPolicy:
    create: true
    name: pis
    spec: {}   # your policy
```

Kustomize users can build on `config/default` the same way:
`resources: [github.com/conallob/raspberrypi-operator//config/default?ref=<tag>]`.

## Development

The CRDs and the manager's ClusterRole in `templates/` are generated from
`config/` (`make chart-sync`) and must not be edited. `make chart-test` lints
the chart, renders it in several configurations, and checks the output against
`config/` (CRDs, RBAC, admission policy, the sample policy), that every
reference between objects resolves, and that the values schema rejects mistakes.
