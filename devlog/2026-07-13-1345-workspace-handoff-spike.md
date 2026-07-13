---
run: manual
stage: ward-runtime-spike
date: 2026-07-13
branch: docs/workspace-handoff-spike
---

# Apple container workspace handoff spike

## Decisions

- **Chose `fresh_vm_read_only_volume_handoff` for the Apple container 1.1.0
  ward class** over the declared same-VM fallback because a dedicated named
  workspace volume survived destruction of the credential-bearing VM and
  mounted read-only in a different VM whose mount list omitted credentials.
- **Capability declaration is qualified:** detachable workspace, read-only
  remount, and composed post-exit export are supported; live credential-volume
  detach and public workspace snapshots are not. Native rootfs export excludes
  named-volume contents, so the trusted exporter must stage the workspace in
  its fresh rootfs or emit the gauntlet manifest and blobs directly.
- **Rejected the private `volume.img` layout as a 1A.2 dependency.** An APFS
  clone mounted and read correctly, proving the block-image substrate, but
  Apple exposes no supported arbitrary-image import or named-volume snapshot
  API in this release.
- **Rejected guest `umount` as credential detach.** With `CAP_SYS_ADMIN`, the
  filesystem unmounted but `/dev/vdd` remained attached and remountable in the
  same VM; the fake credential marker was readable again.

## Gotchas

- A second read-only attachment can coexist, while a second VM is rejected if
  the first holds the volume read-write. Ward conformance must observe writer
  termination before exporter creation rather than trust scheduling intent.
- The exporter received Apple container's default network. This spike proves
  credential and host-filesystem separation, not a network-free export class.
- The runtime was absent at session start; the user installed Apple's signed,
  notarized 1.1.0 package before the conformance run.

## Verification

- The writer saw credentials as ext4 `ro` and workspace as ext4 `rw`; the
  exporter saw only workspace as ext4 `ro`, failed its write probe, and staged
  both workspace markers into a fresh rootfs.
- The exported archive contained both markers and no fake credential marker;
  a separate credential-only VM proved the marker still existed. Same-VM
  remount, native-export omission, unsupported snapshot, and concurrent-writer
  negative probes all produced the expected refutations.
- All uniquely named test containers and volumes were deleted. The installed
  runtime service and pulled Alpine image were left intact.

Re-defers the license ADR-candidate from
`2026-07-08-1051-scaffold-phase0.md`: Phase 4 licensing remains outside this
runtime spike.
