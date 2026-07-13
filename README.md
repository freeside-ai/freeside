# Freeside

**Freeside is a local, durable workflow controller that turns a software work item into an evidence-backed pull request and interrupts me only when judgment is required.**

Category: an agent control plane. Harnesses (Claude Code, Codex) run the agent's inner loop; Freeside is the outer loop: what work starts, inside what boundary, with which credentials withheld, what counts as done, when a human is interrupted, and what survives a crash. The harness runs the agent; the reins are yours.

**Status:** Phase 1A (the secure publish path) underway. The daemon is initialized (Wave 0 unit 1: module, dual-platform CI, test conventions); this monorepo's other component directories stay intentionally empty until their phase begins.

- **Everything** — charter, architecture, roadmap, decisions: [`docs/plan.md`](docs/plan.md).
- **Conventions** — how to work in this repo: [`AGENTS.md`](AGENTS.md).

**License:** none yet. The repository is private and unlicensed while open-sourcing stays a deferred Phase 4 option (plan §11). The leaning, recorded as an ADR-candidate in the first devlog entry, is AGPL-3.0 for the network-service core if and when it is released.

---

A [Free as in Bird](https://freeasinbird.com) project.
