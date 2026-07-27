# project-gh-imgup

The per-project extension of [`agent-claude`](../agent-claude/README.md) for
`freeasinbird/gh-imgup`, the first managed repository (plan §11). It is the agent
base plus this project's dependency closure, baked so that a stage under the
default `provider_only` egress profile reaches no package registry (plan §5.4)
and the clean verifier, which has no network at all (plan §5.6), can still run
the project's verification recipe:

```text
npm ci && npm run lint && npm run typecheck && npm test
```

Build it with `scripts/build-project-gh-imgup-image.sh` (after the base), and
prove the offline property with `scripts/check-project-offline.sh`.

**This image is itself an agent image**, so it also has to pass
`scripts/check-agent-image.sh`: it can inherit a noncompliant `--base`, or grow
an `ENV`, `WORKDIR`, or `VOLUME` of its own, and the offline proof would not
notice either. Run the allowlist check against the final project image as well
as against the base.

## How the Dependencies Are Baked

`package.json` and `package-lock.json` are **vendored here** from upstream commit
`6ab4e3dff2be53f74bde9b8b3150290775152f9f`, rather than fetched during the build:
a build-time fetch would hide from review what is actually baked. The build runs
a real `npm ci` from them to warm an npm cache at `/opt/freeside/npm-cache`,
which is what proves the cache holds every tarball the lockfile resolves,
including this project's platform-native optional dependencies on `linux/arm64`
(`@biomejs/cli-linux-arm64`, which declares glibc, and the native `@typescript/*`
build). The installed tree is then discarded; the workspace installs its own from
the cache.

A global npmrc at `/usr/local/etc/npmrc` points npm at that cache and sets
`prefer-offline`, `audit=false`, `fund=false`, and `update-notifier=false`. It is
a file rather than environment variables because the image may carry no `ENV`
(see the base image's README). `prefer-offline` rather than `offline` so a run
that legitimately has the registry degrades to an ordinary network failure
instead of a confusing cache miss.

The recipe therefore runs **verbatim**: nothing about it is rewritten for the
offline case, because a rewritten recipe would prove a different recipe.

## The Offline Envelope

`scripts/check-project-offline.sh` clones the project at the pinned commit and
runs the recipe twice: once under `--network none`, which must pass, and once
more with the baked cache masked by an empty tmpfs, which must fail. The second
run is what makes the first mean something.

Known limits:

- **The proof is bound to the vendored lockfile.** A candidate branch that adds
  or bumps a dependency fails `npm ci` offline. That is the correct loud failure;
  refreshing the vendored manifests and rebuilding the image is the response.
- **One Node line is proved.** The image carries the base's Node 24; gh-imgup's
  own CI also tests Node 22, and a green in-image run says nothing about that leg.
- The base is consumed by tag (`freeside-agent-claude:local` by default), because
  the builder VM cannot reach a loopback temporary registry and Apple `container`
  1.1.0 cannot resolve a locally built `name@digest`. The base's digest is
  recorded in the image as the `ai.freeside.base.digest` label instead.
