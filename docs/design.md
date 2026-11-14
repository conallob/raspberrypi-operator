# Design

## Goals

- Declare the EEPROM firmware version and bootloader config (initially
  `BOOT_ORDER` plus arbitrary other keys) for Raspberry Pi 4/CM4 and Pi 5/CM5
  nodes, and have the cluster converge to it.
- Make a bad change hard to roll out. The worst failure is a node that cannot
  boot and needs hands-on recovery, so the design prefers refusing to act.
- Standard libraries only (controller-runtime, client-go, `kubectl/pkg/drain`,
  `x/sys`); no subprocesses.

Non-goals for now: `config.txt` tuning, signed boot / OTP, VL805 (Pi 4 USB
controller) firmware, A/B EEPROM updates on Pi 5, resolving release channels.

## Objects

| Kind | Scope | Written by | Purpose |
|---|---|---|---|
| `BootloaderPolicy` | cluster | user | desired firmware + config for a node selector |
| `NodeBootloader` | cluster, named after the node | controller (spec), agent (status) | per-node target, approvals and observed state |

`NodeBootloader.spec` carries a copy of the policy target plus two hashes the
controller sets: `approvedHash` (may stage) and `rebootHash` (may reboot). Both
must equal the hash of the current target, so a policy edit revokes them.

## Update flow

```
agent:       Pending ─────────────► Staged ─────────────► Rebooting ──► UpToDate
                   ▲ approvedHash        ▲ rebootHash
controller:  cordon + approve        drain, then authorise
```

1. The agent probes the node every 30s and compares build timestamp and the
   managed config keys against the target.
2. Pre-flight (see README). Any failure → `Blocked`, nothing written.
3. When `approvedHash` matches, the agent downloads the image, verifies its
   SHA-256, merges the **running** config with the managed keys (so unmanaged
   settings survive), writes the merged config into the image, and stages
   `pieeprom.upd` then `pieeprom.sig` atomically. The signature is the commit
   marker.
4. The controller drains the node, then sets `rebootHash`.
5. The agent records `rebootingFromBootID` in status, **then** calls `reboot(2)`.
6. After boot the bootloader's self-update applies the staged image. The agent
   sees a new boot ID: if the node now matches → `UpToDate` (leftover staged
   files removed); if not → `Failed`, sticky for that target hash.

### Rollout rules (`internal/plan`)

Pure function over node snapshots, covered by unit tests:

- at most `maxUnavailable` nodes approved or cordoned at once, in name order;
- never trust agent status computed for an older target hash;
- a `Failed` node, or a node that doesn't return within `rebootTimeoutSeconds`,
  **halts** the rollout (`RolloutHalted` condition) and leaves that node
  cordoned for a human;
- `paused` stops new approvals but lets in-flight nodes finish;
- a node that becomes `Blocked` after approval has its approval revoked and is
  uncordoned.

## Pi-specific details

- **Image format.** `internal/eeprom` is a Go port of the section parser and
  `bootconf.txt` replacement in `rpi-eeprom-config`
  ([source](https://github.com/raspberrypi/rpi-eeprom)). Images are 512 KiB
  (BCM2711) or 2 MiB (BCM2712). Edited images are re-parsed and the config
  round-tripped before anything is written. `TestRealImages` runs the editor on
  every `default`/`latest` image in an `rpi-eeprom` checkout (~30 at the time of
  writing): each parses, rewriting a config with itself is byte-identical, and a
  real edit changes only a ~20–30 byte window inside the writable region.
- **Build timestamps are per chip.** The two SoCs build one release at different
  times (2026-10-30: `1793402435` for BCM2711, `1793393754` for BCM2712), so
  `buildTimestamp` lives on each image. `versions.txt` in `rpi-eeprom` lists them.
- **Running config** is read from the `blconfig` nvmem device
  (`/sys/firmware/devicetree/base/aliases/blconfig` → `/sys/bus/nvmem/devices/*`),
  the same source `rpi-eeprom-update` prefers over `vcgencmd`.
- **Running build** is `/proc/device-tree/chosen/bootloader/build-timestamp`
  (big-endian).
- **Boot device** is derived from the mount behind the boot partition:
  `mmcblk*` → SD (also CM4 eMMC), `nvme*` → NVMe, `sd*` → USB, NFS → network.
- **Update mechanism** is the bootloader's self-update (`pieeprom.upd` +
  `pieeprom.sig` with a `ts:` and `target-soc:` line), supported since build
  `1650968668`. It avoids `recovery.bin`, which on BCM2711 can only be loaded
  from the SD slot.

## Security notes

- The agent is privileged with `hostPID` (needed for sysfs, the boot partition
  and `reboot(2)`). It can only reboot when the controller has set a matching
  `rebootHash`.
- RBAC can't scope an agent to its own node's `NodeBootloader`. A compromised
  node could write another node's status (e.g. report `UpToDate`). Mitigation
  options: a `ValidatingAdmissionPolicy` tying the status writer's node to the
  object name.
- Images must be `https://` and are pinned by SHA-256. The operator does not
  validate that an image is an *official* Raspberry Pi build.

## Limitations

- **Not tested on hardware.** The image editor is checked against real upstream
  images, but not that a bootloader accepts and applies what it writes. Also
  unverified: the exact boot-mode digit set for `BOOT_ORDER`, the nvmem lookup on
  real kernels, and CM4 eMMC boot behaviour.
- `versions.txt` also records a manufacturing version (`mfg_ver`) per image;
  `rpi-eeprom-update` treats lowering it specially (it needs
  `bootloader_allow_mfg_downgrade=1`). The operator does not model this yet.
- Bootloaders older than build `1650968668` need a one-off manual update.
- If `/boot/firmware` isn't a mount of the device the Pi boots from (some
  distros, netboot), staged files won't be seen by the bootloader. The agent
  detects an update that didn't apply (→ `Failed`) but can't predict it.
- `reboot(2)` is immediate; the node is drained but kubelet isn't stopped
  gracefully.
- Firmware images are pinned explicitly; there is no channel resolution
  (`stable`/`latest`) yet. A config-only change re-flashes the pinned image.
- Draining blocks one reconcile for up to `drainTimeoutSeconds`.
- Pi 5 AB/"immediate" updates over SPI are not used.

## Ideas

- Resolve `channel: stable` to a release and pin the digest in status.
- `config.txt` management via the same approve/stage/reboot machinery.
- Admission policy for agent status writes; kured-style reboot coordination;
  Prometheus metrics for phase and bootloader age.
