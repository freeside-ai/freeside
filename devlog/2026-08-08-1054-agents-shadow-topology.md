# Derive the Workspace Agents Shadow from the Attested Tree

Chose to make the workspace-local `.agents` shadow mount conditional on a
runtime observation of the candidate tree, attested in the workspace
observer's nonce-bound proof (`workspace_agents=dir|absent|other`), over
three rejected alternatives. Apple container creates missing nested
mountpoints only under writable parents; the candidate workspace is mounted
read-only, so a candidate without `.agents` failed `container start` with
errno 30. The live fixture masked this by always pre-creating `.agents`.

Rejected: injecting an empty `.agents` into the candidate volume (the
workspace proof digests directories, so injection would change the exact
tree ward attests); deriving presence from a host-side stat of the staging
checkout (indirect, prep-time-only, and trusts a mutable host path where
the observer proves the volume itself through the reviewer's own read-only
access class, at launch and again pre-start); relaxing the workspace mount
to writable or an overlay (breaks the read-only reviewer isolation).

The ambient shadows (the reviewer's home and every in-container ancestor of
the workspace) sit on the writable rootfs and stay unconditional. Only the
one target inside the read-only mount is conditional, and the read-only,
identity-pinned workspace cannot grow a `.agents` after observation. A
`.agents` that is a symlink or any non-directory kind fails closed before a
credential-bearing container exists: no shadow semantics are defined for it,
and seeding already refuses irregular entries, so it can only mean an
unexpected tree.

The observed entry is bound into the durable journal binding
(`workspace_agents_entry`) beside the already-stored shadow-target list;
shape validation requires the two to agree with the derivation, spec
validation requires the binding to agree with the live observation, and
reconstruction re-observes and compares, so a rebuilt review cannot choose a
different topology than the one bound at launch. The topology version bumps
to `codex_review_read_only_v3`; v2 bindings authenticate teardown only,
mirroring the legacy instruction-binding posture, since a pre-fix binding
must still be reapable but can never launch or satisfy a result.

Verification: deterministic fake-runtime lifecycles for both entry values
through `started` plus a fail-closed `other` refusal; proof-parser
fail-closed cases (omitted, repeated, unknown, empty); binding mismatch
rejections; a host `sh` execution of the actual probe script over
directory, absent, file, and symlink shapes; and the Apple-container live
lifecycle now parameterized over candidates with and without `.agents`,
the absent case crossing final reconstruction and a real `container start`
(both live cases passed on container 1.1.0).

The refute-first pass raised two findings taken as fixes: `verifyBaseProof`
now rejects an extra expectation that collides with a base proof key
(previously a future caller's bad map could silently replace the nonce or
tree expectation), and the probe script gained the host execution test
above so the fake's Go synthesis is tied to the script's real behaviour.
Rejected by verification: proof smuggling via repeated keys, CRLF variants,
or the agents key in other observers' proofs (dual parsers must both
accept, and non-review proofs reject the key as unknown); topology
divergence between built, validated, persisted, and reconstructed target
sets (one derivation feeds all four, realized mounts are compared by
`sameMounts`, and the reloaded binding is byte-compared); legacy v2
authority reaching launch or results (hard v3 requirement outside the
teardown flag); and the absent-then-created TOCTOU (read-only mounts,
exclusivity checks, and the fresh pre-start observation comparing the
entry). Accepted by decision: the pre-existing collection sentinel where an
empty approved image selects teardown validation, unchanged in kind by
this unit and reachable only by bypassing config validation.

Revisit when Apple container learns to create mountpoints under read-only
mounts (the conditional branch could collapse), or when #339-style symlink
carriage changes which entry kinds a candidate tree may legitimately hold.
