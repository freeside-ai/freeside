# daemon

`freesided`, the Go daemon: event inbox, workflow engine, signet (attention service), StageDriver and ReviewSource, ward (runner layer), gauntlet (hostile import and clean verification), git/publish service, store, and sync API. It owns workflow state and all credentials; clients are thin (see `docs/plan.md` §5.1, §5.2).

Daemon CI builds and tests on **Linux as well as macOS from day one**: the daemon core takes no Apple-only dependencies, making portability continuously verified rather than aspirational (plan §3.3).

- **Toolchain:** Go (single static binary, supervised by launchd/systemd, dedicated user).
- **Scope boundary:** daemon-side code only. The daemon/client contract is defined in `api/`; server-side code implementing it lives here, never hand-authored to diverge from the spec.
- **Status:** empty until Phase 1A. Per-component build/test/run commands land in `AGENTS.md` with this component's first PR.
