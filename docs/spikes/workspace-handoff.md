# Apple container workspace handoff spike

- Status: complete
- Tested: 2026-07-13
- Plan target: `docs/plan.md` §5.7, Phase 1A.2 ward gate

## Conclusion

Apple container 1.1.0 can satisfy the strong §5.7 handoff on this machine,
provided the workspace is a dedicated named volume and export happens in a
new container VM. The proven sequence is:

1. mount the workspace read-write and a separate credential volume read-only
   in the agent VM;
2. let the agent process exit, verify the container is stopped, and delete the
   credential-bearing container;
3. create a different container whose mount allowlist contains only the
   workspace volume with `readonly`;
4. have a trusted helper in that VM copy the workspace into its fresh root
   filesystem, then export the stopped helper container.

The exporter saw the workspace as `/dev/vdc /workspace ext4 ro`, a write probe
failed, its inspected configuration contained no credential or host-directory
mount, and the exported archive contained the agent's files. The archive did
not contain the fake credential marker, while a separate audit VM proved that
the marker still existed in the detached credential volume.

**Recommendation:** 1A.2 may declare a strong class named
`fresh_vm_read_only_volume_handoff` for Apple container 1.1.0. It must not
declare the weaker same-VM fallback, hot credential detachment, or workspace
snapshot support. The class is conditional on the conformance checks under
[Required backend contract](#required-backend-contract).

This proves credential and host-filesystem separation for the exporter. It
does not prove a network-free exporter: Apple container attached the default
network in this test. It also cannot stop an agent from deliberately copying a
credential into the workspace before exit; §5.4's scanning and opaque-artifact
controls remain necessary.

## Runtime under test

| Item | Observed value |
| --- | --- |
| Host | Mac Studio `Mac13,1`, Apple M1 Max, 32 GB |
| OS | macOS 26.5.1 (25F80), arm64 |
| Runtime | Apple container 1.1.0, release commit `5973b9cc626a3e7a499bb316a958237ebe14e2ed` |
| Package trust | Developer ID Installer: Apple Inc. - Containerization (`UPBK2H6LZM`); notarized |
| Runtime state root | `$HOME/Library/Application Support/com.apple.container/` |
| Test image | `docker.io/library/alpine:3.22` (Alpine 3.22.5 arm64 manifest `sha256:2c9d26f410d032d5b1525aa8a873e238b05b90c4ae8618743d4311f0cc827e37`) |

Apple documents that `container` runs one lightweight VM per container and
mounts only the host data selected for that VM. The 1.1.0 source also shows
that named volumes are host-side ext4 `volume.img` files. References are
pinned to the tested release:

- [technical overview: one VM per container](https://github.com/apple/container/blob/5973b9cc626a3e7a499bb316a958237ebe14e2ed/docs/technical-overview.md#L22-L30)
- [volume service: host block-file storage](https://github.com/apple/container/blob/5973b9cc626a3e7a499bb316a958237ebe14e2ed/Sources/Services/ContainerAPIService/Server/Volumes/VolumesService.swift#L29-L44)
- [volume creation: ext4 image and recorded source](https://github.com/apple/container/blob/5973b9cc626a3e7a499bb316a958237ebe14e2ed/Sources/Services/ContainerAPIService/Server/Volumes/VolumesService.swift#L290-L353)
- [container export implementation](https://github.com/apple/container/blob/5973b9cc626a3e7a499bb316a958237ebe14e2ed/Sources/ContainerCommands/Container/ContainerExport.swift#L23-L71)

## Capability result

| §5.7 capability | Result on 1.1.0 | Evidence and boundary |
| --- | --- | --- |
| `supports_detachable_workspace` | **Yes** | A named workspace volume survived writer VM exit and container deletion, then attached to a different VM. |
| `supports_post_exit_export` | **Yes, composed** | `container export` exported a stopped container root filesystem. Named-volume contents are excluded, so the fresh helper first copied the read-only workspace into its own root filesystem. There is no direct named-volume export command. |
| `supports_read_only_remount` | **Yes** | The same workspace mounted `rw` in the writer and later `ro` in the exporter. `/proc/mounts` reported `ro`; `touch` failed with `Read-only file system`. |
| `supports_credential_volume_detach` | **No for a live VM** | There is no hot-detach command. Default guests could not `umount`; with `CAP_SYS_ADMIN`, guest unmount succeeded but `/dev/vdd` remained attached and could be remounted, exposing the marker again. VM destruction plus omission in a new VM is the supported boundary. |
| `supports_workspace_snapshot` | **No public support** | `container volume` exposes create/delete/list/inspect/prune only. `container volume snapshot` was rejected. Root-filesystem export omitted mounted volume contents. |

`supports_post_exit_export` should be read narrowly: the runtime can export a
stopped root filesystem, and that primitive composes with the separate export
VM. It must not be interpreted as direct export of a stopped container's
mounted workspace volume.

## Candidate mechanisms

### Detachable named volume: pass, use this

The writer observed two different block devices:

```text
/dev/vdd /credentials ext4 ro,relatime 0 0
/dev/vdc /workspace   ext4 rw,relatime 0 0
```

After the writer stopped and was deleted, a fresh container mounted the same
workspace volume as:

```text
/dev/vdc /workspace ext4 ro,relatime 0 0
```

The new container's inspected mount list contained exactly one entry,
`/workspace` with `options: ["ro"]`. `ssh` was false, its environment contained
only the image `PATH`, and no host directory, published socket, or credential
volume appeared.

A second VM cannot attach a volume while another VM holds it read-write. The
attempt failed during Virtualization.framework bootstrap with
`VZErrorDomain Code=2`, "The storage device attachment is invalid." Concurrent
read-only attachments did succeed. The ward must therefore verify writer
termination before it creates the exporter; it cannot use scheduling intent as
proof.

### Separate export VM: pass, use this

The exporter asserted both workspace files, absence of `/credentials` and
`/Users`, and a failed write probe. It copied the workspace into `/handoff` in
its fresh root filesystem. Native post-exit export produced an 8,829,952-byte
archive with SHA-256:

```text
7106af42cc3cd20fe5e1af936ad91f00802736fa6d2c56104b7210e5d1b8c268
```

Relevant archive contents and values were:

```text
handoff/result.txt          agent-output
handoff/nested/state.txt    durable-workspace
handoff-proof.txt           credentials=absent
                            host_home=absent
                            workspace_write=blocked
```

Searching the archive for
`FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT` returned no match. A later
VM mounting only the credential volume read that exact marker, proving absence
from the export was caused by mount omission rather than deletion.

The whole-rootfs tar is sufficient to prove the runtime composition, not the
desired gauntlet format. Production should run a trusted export helper that
emits only normalized content blobs and a manifest, as §5.6 requires.

### Snapshot / native export: partial primitive, not a handoff

`container export` succeeded after the writer exited. The 8,825,856-byte
rootfs archive had SHA-256
`bd601ea2b29897ca0fc5a044e4fe91af03bf513d3ba7344188c08f76bedddd16`.
It contained empty `workspace/` and `credentials/` mountpoint directories, but
none of `result.txt`, `state.txt`, `agent-output`, `durable-workspace`, or the
fake credential marker. Native export therefore does not capture a workspace
kept on a named volume.

There is no public named-volume snapshot or export command in 1.1.0. Image
snapshots used internally for OCI layers are not workspace snapshots and do
not satisfy `supports_workspace_snapshot`.

### Host-controlled block image: substrate works, public mechanism absent

`container volume inspect` exposed separate host paths ending in
`volumes/<name>/volume.img`; the guest mounted them as ext4. With all VMs
detached, the spike APFS-cloned the workspace `volume.img`, atomically replaced
the block file of a newly created, spike-labeled volume, and mounted that clone
read-only in another VM. Source and clone both hashed to:

```text
1a52d2d2337521e75e776b49ab42809bc24ea33f0260cacbca8949f8f5fd6c6b
```

The clone contained all workspace markers and no credential path. This proves
the storage substrate can support a host-controlled block-image design.
However, the procedure mutates Apple container's private state: the public CLI
cannot create a volume from an arbitrary image, import a clone, or snapshot a
volume. 1A.2 should not depend on this layout or advertise it as a supported
runtime capability. Revisit only through a public API or a Freeside-owned
Containerization integration with its own conformance contract.

### Same-VM export after credential detach: fail, do not declare

The fallback kept one VM alive, mounted workspace `rw` and credentials `ro`,
ran an agent process through `container exec`, and let that process exit. The
default container lacked permission to unmount `/credentials`.

A second run added `CAP_SYS_ADMIN` to test the strongest plausible guest-only
version. `umount /credentials` removed the filesystem mount, but the VM still
held `/dev/vdd`. A later process in the same VM mounted `/dev/vdd` at a new
path and reread the fake credential:

```text
guest_mount=absent
block_device=/dev/vdd
block_device=still_attached
credential_marker=readable_after_guest_unmount
```

A guest unmount is not a credential-device detach. Release 1.1.0 exposes no
host hot-detach operation, so the same-VM isolation class is unavailable.

## Required backend contract

Set `fresh_vm_read_only_volume_handoff` only when all of these checks pass:

1. Provision the workspace as its own named volume. Put every provider
   credential in a different mount. Never place a credential in the container
   root filesystem or workspace.
2. Keep `container` CLI/XPC control outside the agent VM. Do not mount the host
   CLI, runtime sockets, daemon state, SSH agent, home directory, or registry
   credentials into it.
3. On agent completion, require observed `state: stopped`, then delete the
   container and verify its ID is absent from `container list --all`. A timeout
   or deletion failure fails the gate.
4. Create the exporter from a pinned image with a generated mount allowlist.
   Inspect before execution: exactly one persistent mount, the expected
   workspace volume at the expected target with `ro`; no credential volume,
   host bind, SSH forwarding, environment credential, or published socket.
5. In the exporter, verify `/proc/mounts` says `ro`, a write probe fails, and
   expected credential and host paths are absent. Treat any mismatch as a
   conformance failure.
6. Run only the trusted export helper. Do not execute workspace code. Emit the
   §5.6 regular-file manifest and content blobs, then destroy the exporter.
7. Verify exported digests and scan the output under §5.4 before it reaches the
   out-of-process gauntlet worker.

Conformance should retain the negative probes: reject export while the writer
holds the volume read-write; prove a fake credential remains absent from the
archive while present in its detached volume; and prove same-VM guest unmount
does not count as detach.

## Reproduction

All credential text below is an inert marker. Use unique names; cleanup deletes
only those names.

```sh
container --version
container system start --enable-kernel-install
container system status --format json

IMAGE=docker.io/library/alpine:3.22
WS=freeside-handoff-ws-repro
CRED=freeside-handoff-cred-repro
AGENT=freeside-handoff-agent-repro
EXPORTER=freeside-handoff-exporter-repro
OUT=/tmp/freeside-handoff-exporter-rootfs.tar

container image pull "$IMAGE"
container volume create -s 64M --label freeside.spike=workspace-handoff "$WS"
container volume create -s 8M --label freeside.spike=workspace-handoff "$CRED"

container run --rm --name freeside-handoff-cred-seed-repro \
  --mount type=volume,source="$CRED",target=/credentials \
  "$IMAGE" sh -c \
  'printf "%s\n" FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT > /credentials/token; chmod 0400 /credentials/token; sync'

container run --name "$AGENT" \
  --mount type=volume,source="$WS",target=/workspace \
  --mount type=volume,source="$CRED",target=/credentials,readonly \
  "$IMAGE" sh -c \
  'set -eu; test "$(cat /credentials/token)" = FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT; printf "%s\n" agent-output > /workspace/result.txt; mkdir -p /workspace/nested; printf "%s\n" durable-workspace > /workspace/nested/state.txt; sync; grep " /credentials " /proc/mounts; grep " /workspace " /proc/mounts'

container inspect "$AGENT"
container export --output /tmp/freeside-handoff-agent-rootfs.tar "$AGENT"
tar -tf /tmp/freeside-handoff-agent-rootfs.tar | grep -E 'workspace|credentials|result.txt|state.txt'
container delete "$AGENT"
container list --all --format json

container run --name "$EXPORTER" \
  --mount type=volume,source="$WS",target=/workspace,readonly \
  "$IMAGE" sh -c \
  'set -eu; test -f /workspace/result.txt; test -f /workspace/nested/state.txt; test ! -e /credentials; test ! -e /Users; grep " /workspace " /proc/mounts; if touch /workspace/must-not-write 2>/tmp/write-error; then exit 92; fi; grep -q "Read-only file system" /tmp/write-error; mkdir /handoff; cp -a /workspace/. /handoff/; printf "%s\n" credentials=absent host_home=absent workspace_write=blocked > /handoff-proof.txt; sync'

container inspect "$EXPORTER"
container export --output "$OUT" "$EXPORTER"
tar -tf "$OUT" | grep -E '^handoff|^handoff-proof.txt'
tar -xOf "$OUT" handoff/result.txt
tar -xOf "$OUT" handoff/nested/state.txt
tar -xOf "$OUT" handoff-proof.txt
if grep -a -q FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT "$OUT"; then
  echo 'credential marker escaped' >&2
  exit 1
fi

container run --rm --name freeside-handoff-credential-audit-repro \
  --mount type=volume,source="$CRED",target=/credentials,readonly \
  "$IMAGE" sh -c \
  'test "$(cat /credentials/token)" = FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT'

container delete "$EXPORTER"
```

Same-VM refutation, using only the fake volumes above:

```sh
SAMEVM=freeside-handoff-samevm-repro
container run --detach --cap-add CAP_SYS_ADMIN --name "$SAMEVM" \
  --mount type=volume,source="$WS",target=/workspace \
  --mount type=volume,source="$CRED",target=/credentials,readonly \
  "$IMAGE" sleep 300

container exec "$SAMEVM" sh -c \
  'grep " /credentials " /proc/mounts | cut -d " " -f 1 > /tmp/credential-device; umount /credentials; cat /tmp/credential-device'
container exec "$SAMEVM" sh -c \
  'mkdir /credential-remount; mount -t ext4 -o ro "$(cat /tmp/credential-device)" /credential-remount; test "$(cat /credential-remount/token)" = FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT'

container stop "$SAMEVM"
container delete "$SAMEVM"
```

The host-controlled block-image test also runs before the final workspace and
credential cleanup. It deliberately replaced the block file of a new,
spike-labeled volume. It is evidence, not a supported recipe, and should not be
automated against user volumes:

```sh
CLONE=freeside-handoff-ws-clone-repro
container volume create -s 64M --label freeside.spike=workspace-handoff "$CLONE"
SRC="$HOME/Library/Application Support/com.apple.container/volumes/$WS/volume.img"
DST="$HOME/Library/Application Support/com.apple.container/volumes/$CLONE/volume.img"
cp -c "$SRC" "$DST.next"
mv -f "$DST.next" "$DST"
shasum -a 256 "$SRC" "$DST"
container run --rm --mount type=volume,source="$CLONE",target=/workspace,readonly \
  "$IMAGE" sh -c 'cat /workspace/result.txt; test ! -e /credentials'
container volume delete "$CLONE"
```

Final cleanup after any optional probes:

```sh
container volume delete "$WS" "$CRED"
```

## Open boundary

The exporter in this spike had the runtime's default network. Section 5.7 does
not make networklessness part of the handoff sentence, but Freeside should not
silently infer it from this result. If ward policy requires a network-free
export helper, prove and name that as a separate capability before unattended
use.
