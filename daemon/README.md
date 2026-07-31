# daemon

`freesided`, the Go daemon: event inbox, workflow engine, signet (attention service), StageDriver and ReviewSource, ward (runner layer), gauntlet (hostile import and clean verification), git/publish service, store, and sync API. It owns workflow state and all credentials; clients are thin (see `docs/plan.md` §5.1, §5.2).

Daemon CI builds and tests on **Linux as well as macOS from day one**: the daemon core takes no Apple-only dependencies, making portability continuously verified rather than aspirational (plan §3.3).

- **Toolchain:** Go (single static binary, supervised by launchd/systemd, dedicated user). Module `github.com/freeside-ai/freeside/daemon`, pinned in `go.mod`; build/test/run commands are in `AGENTS.md`.
- **Scope boundary:** daemon-side code only. The daemon/client contract is defined in `api/`; server-side code implementing it lives here, never hand-authored to diverge from the spec.
- **Status:** initialized in Phase 1A (Wave 0 unit 1). `internal/` holds one placeholder package per lane (`signet`, `export`, `importer`, `verify`, `publish`, `ward`, `domain`, `engine`); each lane's real code lands with its Wave unit.

## Testing conventions

**Golden files.** Tests that assert a serialized shape compare it against a
committed fixture rather than hand-writing the expected bytes inline. Use the
shared helper `internal/golden` so every lane's golden tests share one shape
and one regeneration switch:

```go
import "github.com/freeside-ai/freeside/daemon/internal/golden"

func TestRender(t *testing.T) {
    got := render(input)          // []byte
    golden.Assert(t, "render", got) // vs testdata/render.golden
}
```

- Fixtures live in the test package's own `testdata/` directory, named
  `<case>.golden` (the `name` passed to `Assert`).
- Regenerate after an intended change with the package-level `-update` flag,
  then review and commit the diff:

  ```sh
  go test ./internal/foo -run TestRender -update
  ```

`internal/golden` and its `golden_test.go` are the worked example.

## GitHub App Credential Onboarding

The default publish identity is one public GitHub App owned by the operator's
personal account. A fresh operator registers it through GitHub's manifest flow;
Freeside generates the suggested name, pins the publish permission set, and
writes the conversion key directly to the protected credentials directory.

Repository onboarding uses GitHub's native installation page:
`https://github.com/apps/<app-slug>/installations/new`. Select only the
repository being onboarded. For an organization, GitHub may turn that action
into a request for an organization owner's approval; after approval, resume
onboarding and Freeside detects the installation through canonical
App-authenticated discovery.

Every machine uses a distinct private key within the same registration. On a
new machine, open the App's personal-account settings page, generate a private
key, and import the downloaded PKCS#1 PEM. Freeside authenticates the key
against the recorded numeric App ID, owner, canonical name and slug, and
visibility before protected storage accepts it, then records the same SHA-256
public-key fingerprint GitHub displays. Delete the downloaded PEM after the
import succeeds. Copying a PEM between machines is outside the contract because
it prevents independent machine revocation.

Publish-credential doctor checks cover the keystore layout and owner-only
modes, expected per-registration key presence, canonical visibility metadata,
active janitor coverage for every registration, and the former singleton
layout. Reusing one key across multiple local registrations is also reported,
the PEM-copy pattern that can be detected from one machine.

## Operational Commands

`freesided setup -operator <login> -operator-id <id>` creates the Phase 1A
single-directory layout and the canonical empty installation-authority
document. The default is `~/.freeside`; `-config-dir` selects another root.
The first pass returns GitHub's manifest form for the operator's public
personal App. After GitHub returns the one-time code, repeat setup with
`-registration-code <code>`; Freeside verifies GitHub's canonical App,
visibility, permissions, and owner identity, stores the private key directly
in the protected keystore, and creates the registration's fail-closed
authority entry. A replay validates existing authority without overwriting it,
and existing credentials without matching authority fail closed rather than
silently authoring an empty destructive installation set.

Every state directory is owner-only, including the corrected
`<db>.fake-stage-driver` path required by the attended publication bootstrap.
The static binary and supervisor may be installed through a one-time narrow
elevation step, but the stateful daemon always runs as the non-root operator.

`freesided onboard <owner/name>` packages the previously manual path. It:

1. resolves the repository ID through exactly one selected installation across
   every local App registration in the operator-authored authority document,
   refusing a second App binding that runtime resolution would find ambiguous;
2. records a bounded, non-authorizing pending installation or
   repository-expansion envelope, returns the canonical native GitHub
   installation route, and resumes the same envelope with `--resume`;
3. mints a pending-gated, repository-scoped read-only token, uses it for
   private-repository identity and clone requests, audits the live `-base-ref`,
   requires its resolved commit to equal `-commit`, and retains that exact
   evidence;
4. derives a conservative profile from the fresh retained audit and returns
   the installation- and image-request-bound approval digest as
   `review_required`;
5. accepts only `-approve <approval_digest>` from that review; and
6. invokes `internal/projectimage` directly, requires the returned preparation
   command and registry/image destination to match the approved request, then
   rechecks the exact authority and reconciled grant before activating the
   profile; a pending installation is promoted only after the image has been
   durably recorded and the review accepted.

The selected npm recipe is detected at `.freeside/verify.json` under `-source`,
or may be supplied explicitly with `-recipe`. Run the command once without
`-approve`, inspect the complete JSON review, then repeat it with the returned
approval digest. A changed workflow head or installation coordinate produces a
different review, as does replacement of the bounded pending intent, and
invalidates the earlier approval. That is the one Freeside manual review; a
GitHub organization approval remains a separate native prerequisite.

`freesided doctor -db <path> -backend-configuration-digest <digest>` reports
the current config-bound conformance and workspace-handoff declaration plus
checkpoint encryption, checkpoint currency, artifact closure, and restore-test
age. Pass each current policy digest as a repeatable `-approved-recipe`; the
same set gates both artifact reconstruction and checkpoint closure. Each
unhealthy dimension converges on one blocking `system_health` item; a later
healthy pass resolves that item. Production driver mode supplies its live
backend digest and runs the same composition at startup and every 24 hours by
default (`-doctor-interval` overrides the cadence). The production daemon
accepts the same repeatable `-approved-recipe` flag and supplies that exact set
to persistence reconstruction and every scheduled doctor pass. Scheduled
conformance holds the janitor's latest completed coverage stable for its
authenticated fetch, so the janitor's deliberate mid-pass withdrawal cannot
terminate the daemon. Doctor applies the selected mode's full capability floor; unattended
health therefore requires the networkless-export and enforced-provider-egress
proofs in addition to the attended handoff floor. Pass `-operating-mode
unattended` to a one-shot doctor for an unattended daemon; the default is
`attended_dev`.

Operational results always state the operating mode and isolation class.
`attended_dev` is the default; unattended operation is an explicit
`-operating-mode unattended` choice. An attended admission resolves the
currently active trust profile at import, where its protected paths are
applied; an unattended admission remains bound to the exact profile digest it
recorded before execution.
