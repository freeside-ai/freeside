# app

The SwiftUI multiplatform client: the macOS + iOS inbox that surfaces the decisions waiting on a human (see `docs/plan.md` §4.8).

- **Toolchain:** Xcode / Swift Package Manager.
- **Scope boundary:** client-side code only. The Swift API client is generated from `api/` (OpenAPI) via swift-openapi-generator; no JS toolchain enters this component.
- **Status:** empty until Phase 2. Per-component build/test/run commands land in `AGENTS.md` with this component's first PR.
