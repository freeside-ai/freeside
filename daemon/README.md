# daemon

`freesided`, the Go daemon: event bus, pipeline engine, ACP session layer, token broker, and API. It owns all state and all agent processes; clients are thin (see `docs/plan.md` §4.2).

- **Toolchain:** Go (single static binary under launchd).
- **Scope boundary:** daemon-side code only. The daemon/client contract is defined in `api/` (OpenAPI); server stubs are generated here from that spec, never hand-authored to diverge from it.
- **Status:** empty until Phase 1. Per-component build/test/run commands land in `AGENTS.md` with this component's first PR.
