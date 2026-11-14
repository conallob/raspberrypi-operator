# raspberrypi-operator

A Kubernetes operator that manages the bootloader EEPROM of Raspberry Pi
nodes: **firmware updates** and **bootloader configuration** (starting with
`BOOT_ORDER`), declared in a manifest and rolled out one node at a time.

| Board | SoC | Image key |
|---|---|---|
| Pi 4, Pi 400, CM4 | BCM2711 | `bcm2711` |
| Pi 5, Pi 500, CM5 | BCM2712 | `bcm2712` |

> **Status: alpha, not yet run on real hardware.** The logic is unit-tested
> against synthetic images and fixture trees, but nothing here has flashed a
> real EEPROM. Try it on a spare node you can recover by hand before pointing it
> at a cluster you care about. See [docs/design.md](docs/design.md#limitations).

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
    buildTimestamp: 1750000000        # what the bootloader reports once updated
    images:
      - {chip: bcm2711, url: "https://…/pieeprom-….bin", sha256: "…"}
      - {chip: bcm2712, url: "https://…/pieeprom-….bin", sha256: "…"}
  config:
    bootOrder: "0xf461"               # NVMe, USB, SD, then restart
    settings: {POWER_OFF_ON_HALT: "1"}
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

## Install

```bash
make install        # CRDs, controller, agent
kubectl label node <name> rpi.conallob.dev/managed=true
kubectl apply -f config/samples/bootloaderpolicy.yaml   # after filling it in
kubectl get bootloaderpolicy,nodebootloader
```

The agent mounts the host's `/boot/firmware`. Distros that mount it elsewhere
(or not at all) need the hostPath in `config/agent/daemonset.yaml` adjusted.

## Development

```bash
make test lint generate manifests
```

Requires Go 1.27.
