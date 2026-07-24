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
