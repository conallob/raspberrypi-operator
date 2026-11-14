# raspberrypi-operator

A Kubernetes operator that manages the bootloader EEPROM of Raspberry Pi
nodes: **firmware updates** and **bootloader configuration** (starting with
`BOOT_ORDER`), declared in a manifest and rolled out one node at a time.

Images are arm64-only, as every supported board is.

| Board | SoC | Image key |
|---|---|---|
| Pi 4, Pi 400, CM4 | BCM2711 | `bcm2711` |
| Pi 5, Pi 500, CM5 | BCM2712 | `bcm2712` |

> **Status: alpha, not yet run on real hardware.** The image editor is checked
> against the genuine `rpi-eeprom` images, and the rest is unit-tested against
> fixture trees, but nothing here has flashed a real EEPROM. Try it on a spare
> node you can recover by hand before pointing it at a cluster you care about. See [docs/design.md](docs/design.md#limitations).

## How it works

```yaml
apiVersion: rpi.conallob.dev/v1alpha1
kind: BootloaderPolicy
metadata:
  name: pis
spec:
  nodeSelector:
    matchLabels: {rpi.conallob.dev/managed: "true"}
  firmware:
    images:                           # from github.com/raspberrypi/rpi-eeprom
      - {chip: bcm2711, buildTimestamp: 1793402435, url: "https://…/pieeprom-2026-10-30.bin", sha256: "…"}
      - {chip: bcm2712, buildTimestamp: 1793393754, url: "https://…/pieeprom-2026-10-30.bin", sha256: "…"}
  config:
    bootOrder: [nvme, usb, sd, restart]   # tried first to last
    powerOffOnHalt: true                  # typed switch for a common key
    settings: {SD_BOOT_MAX_RETRIES: "3"}  # anything else, validated
  rollout: {maxUnavailable: 1}
```

1. The **controller** creates a `NodeBootloader` per selected node.
2. The **agent** (a privileged DaemonSet) probes its node and reports whether it
   differs from the target, running every pre-flight check.
3. The controller approves nodes one at a time: **cordon, stage, drain, reboot**.
   Nothing is written to a node until it is approved, and no node reboots until
   it is drained.
4. After the reboot the agent confirms the new build and config. If the node
   came back but doesn't match, it is marked `Failed`, the rollout halts, and
   the agent will not retry or loop.

Everything is done in Go: no `rpi-eeprom-update`, `vcgencmd` or other
subprocesses. The agent edits the EEPROM image's embedded `bootconf.txt`,
writes `pieeprom.upd` + `pieeprom.sig` to the boot partition for the
bootloader's own self-update, and calls `reboot(2)`.

## Safety checks

A node is **Blocked** (nothing written) when:

- `BOOT_ORDER` can't boot from the device the node is running from, or has no
  bootable entry before a stop/restart, or the current boot device can't be
  determined (override: `safety.allowUnsafeBootOrder`);
- the target is older than the running bootloader (override:
  `safety.allowDowngrade`);
- the running bootloader is too old for self-update, or self-update is disabled;
- the running config can't be read, or a managed key is also set under a
  conditional `[section]`;
- the image can't be downloaded, doesn't match its `sha256`, or isn't a valid
  EEPROM image.

Approval and reboot authorisation are bound to a hash of the target, so editing
the policy invalidates them. A node that the operator cordoned is returned to
service when it succeeds, or when the policy is deleted; a node cordoned by
someone else is never uncordoned.

## A strictly typed API

Mistakes are caught when you apply the manifest, not when a node fails to boot:

- **`bootOrder` is a list of named sources** (`sd`, `usb`, `bcm-usb`, `nvme`,
  `network`, `http`, `rpiboot`, `stop`, `restart`), not a hex string, so the
  digit-order mix-ups of `BOOT_ORDER=0x…` can't happen. The operator writes the
  hex.
- **Typed switches** (`bootUART`, `wakeOnGPIO`, `powerOffOnHalt`,
  `netInstallAtPowerOn`) are real booleans. `settings` takes any other key, but
  rejects bad names (`^[A-Z][A-Z0-9_]*$`), values that aren't printable ASCII,
  duplicates of a typed field, `BOOT_ORDER`, and `ENABLE_SELF_UPDATE` (the update
  mechanism depends on it).
- **CEL rules in the CRD** reject repeated or unknown boot sources, an order with
  nothing bootable, and a **downgrade on edit** unless `safety.allowDowngrade`
  is set. Unknown fields are caught too: `kubectl apply` rejects a typo such as
  `bootOrdr`, where a bare API server would silently drop it.
- **An admission policy** (`config/policy/`) stops an agent writing any node's
  status but its own.

The agent re-checks all of this, since it can't assume the CRD schema installed
in the cluster is current. These rules are tested offline against the generated
CRD using the API server's own validation code (`api/v1alpha1/crd_test.go`).

## Install

```bash
make install        # CRDs, controller, agent
kubectl label node <name> rpi.conallob.dev/managed=true
kubectl apply -f config/samples/bootloaderpolicy.yaml   # pinned to the 2026-10-30 release
kubectl get bootloaderpolicy,nodebootloader
```

The admission policy needs `admissionregistration.k8s.io/v1`
ValidatingAdmissionPolicy (Kubernetes 1.30+) and a token that carries the node
name; see the comments in `config/policy/agent-own-node.yaml`.

The agent mounts the host's `/boot/firmware`. Distros that mount it elsewhere
(or not at all) need the hostPath in `config/agent/daemonset.yaml` adjusted.

## Choosing an image

Take a `pieeprom-*.bin` from
[`rpi-eeprom`](https://github.com/raspberrypi/rpi-eeprom)
(`firmware-2711/` and `firmware-2712/`, normally the `default` release). Its
`buildTimestamp` is the `build_epoch` column of that directory's `versions.txt`;
it differs per chip even for the same release date, so it's set per image.
`config/samples/bootloaderpolicy.yaml` has real, verified values.

## Development

```bash
make test lint generate manifests
# also run against genuine upstream images:
RPI_EEPROM_REPO=/path/to/rpi-eeprom go test ./internal/eeprom -run RealImages -v
```

Requires Go 1.27.
