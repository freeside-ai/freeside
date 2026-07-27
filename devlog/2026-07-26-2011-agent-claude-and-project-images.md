# Shape the Claude Agent Base and the gh-imgup Project Image (#304)

Issue #304 builds the two images #237 consumes: the digest-pinned Claude agent
base plan §5.7 requires to pin CLI versions, and the per-project extension for
`freeasinbird/gh-imgup` whose dependency closure has to be baked because
`provider_only` is the default egress profile (§5.4) and the clean verifier has
no network at all (§5.6). The plan carries no image-shape specification (the
`§4.5` pointer in `images/README.md` was dangling), so the binding constraints
came from measurement against the runtime and from ward's gate, not from prose.

## Measured Constraints That Shaped Both Images

Apple `container` 1.1.0 on macOS 26.5.2, measured with `container create` plus
`container inspect`, which is the path ward's gate uses:

- The image's own `ENV` appears in `initProcess.environment`, and the image's
  `WORKDIR` becomes `initProcess.workingDirectory`. Ward check 2
  (`verifyAgentAllowlist`) compares both against the fixed PATH plus the
  daemon-supplied environment and a working directory of `/`, so an agent image
  may carry neither. This is what rules out the official `node:*` images, which
  set `NODE_VERSION`: the base installs Node from the upstream tarball instead.
- `HOME=/root` is injected by the runtime's init at start, though it is absent
  from the inspected configuration. The CLI therefore has a home directory
  without ward supplying one.

## Decisions

**Chose Debian slim plus an upstream Node tarball over Alpine/musl.** gh-imgup's
lockfile resolves platform-native binaries whose linux-arm64 builds declare
glibc (`@biomejs/cli-linux-arm64`, and the native `@typescript/*` port that
TypeScript 7 ships), so a musl base would exercise a different, unverified set of
artifacts for the project image, and the base has to match the project image it
carries. Rejected: Alpine for consistency with the exporter image, which shares
none of these dependencies.

**Chose to assert the pinned CLI version from the built image, not to record
it.** The build script runs `claude --version` inside the image under
`--network none` and fails unless it reports the pin. A label records what the
build intended; only the run proves what shipped. The image also disables
auto-update through `/etc/claude-code/managed-settings.json`, because a pin that
the first provider-facing run replaces is not a pin, and the image cannot set
`DISABLE_AUTOUPDATER` itself (no `ENV`).

**Chose a baked npm cache plus a global npmrc over baked `node_modules`.** The
project image warms `/opt/freeside/npm-cache` with a real `npm ci` from the
vendored lockfile and configures npm through `/usr/local/etc/npmrc`
(`prefer-offline`, `audit=false`), so the project's verification recipe runs
**verbatim** with no network. Rejected: baking `node_modules` and having the
recipe skip installation, or running `npm ci --offline`; both would prove a
recipe the project does not use. `prefer-offline` over `offline=true` so an
online run that legitimately needs the registry fails as a network error rather
than a confusing cache miss. Consequence, stated as a limit rather than fixed: a
candidate branch that changes dependencies fails `npm ci` offline. That is the
correct loud failure, and refreshing the vendored manifests is the response.

**Chose to vendor gh-imgup's `package.json` and `package-lock.json` over
fetching them at build time.** A build-time fetch would hide from review what is
actually baked and make image contents depend on an unpinned network read.

**Chose a negative probe as part of the offline proof.** The proof runs the
recipe twice: once under `--network none`, which must pass, and once with the
baked cache masked by an empty tmpfs, which must fail. Without the second run, a
recipe that quietly reached a registry, or one that never needed dependencies,
would look identical. tmpfs rather than permissions because the recipe runs as
root, for whom a `chmod` proves nothing.

**Chose to consume the base by tag, with its digest recorded in a label.** The
builder runs in its own VM, so a loopback temporary registry is unreachable from
it, and container 1.1.0 cannot resolve a locally built `name@digest` at all (the
defect `scripts/build-exporter-image.sh` already works around). `FROM` a digest
would simply not build; `ai.freeside.base.digest` carries the provenance the tag
cannot.

**Chose to duplicate the temporary-registry lifecycle rather than extract a
shared helper (owner decision).** The exporter script is live-verified by a
recorded refute-first pass and was this unit's declared non-goal; a sourced
library would have been this repo's first `source`, introduced for two callers.
Now that a third caller exists, the extraction is filed as #324.

**Chose to record, not pin, the Debian package versions (owner decision).** The
cryptographically pinned inputs are the base image digest, the Node tarball
sha256 and the CLI version; `git` and `ca-certificates` versions are written into
an in-image manifest. Rejected: exact apt pins, which turn unbuildable once
Debian drops the superseded version; and `snapshot.debian.org`, whose extra
plumbing and flakiness buy determinism this unit does not need. "Reproducible"
here means pinned inputs plus a recorded digest, not a bit-identical image;
Apple container's build metadata varies between identical invocations
([`2026-07-21-1108-apple-container-exporter-seeding.md`](2026-07-21-1108-apple-container-exporter-seeding.md)).

## Verification Finding That Changes Another Unit

Ward's allowlist compares the observed environment against
`[fixedContainerPathEnv] + spec.Env` **in order**, and container 1.1.0 reports a
supplied environment in nondeterministic order, with the image's own environment
after it (two consecutive creates of the same spec returned `C,A,B` then
`A,C,B`). The gate passes today only because every ward spec in the tree supplies
an empty environment. The first agent spec carrying any environment fails check 2
nondeterministically, which is #237's Claude driver. Filed as #323; it is a
daemon-side comparison fix, out of this unit's `images/`+`scripts/` scope.

Revisit when: the plan gains an image-shape section (#325), a later container
release resolves local `name@digest` references (which would retire both the
temporary-registry seeding and the base-by-tag workaround), or gh-imgup stops
being the first managed repository.

Follow-up: #323, #324, #325.
