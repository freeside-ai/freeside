# Freeside

**Freeside runs coding-agent workflows on your machine, turning tasks into
pull requests and asking you when your judgment is needed.**

Freeside is intended for small fixes as well as larger changes. The goal is
useful, correct work that's worth the attention, maintenance, money, and risk
it costs. Agents can keep working without constant supervision while you
approve specifications and judge results. The agent works in your preferred
harness, such as Claude Code or Codex. You keep your existing subscriptions.

In the intended workflow, Freeside controls when work is ready: it runs
approved checks, arranges independent review, and collects the evidence you
need to judge the result. The daemon, a local background service, keeps GitHub
credentials outside the agent's workspace and publishes the reviewed work as
a pull request. You review and merge it.

Freeside is an agentic development environment (ADE): the place you direct
coding agents from, rather than a tool you write code in. It keeps working
while you're away, and work counts as ready on evidence, not on your watching
it.

*The harness runs the agent; you hold the reins.*

Verification and review apply to the exact code being accepted. When that code
or the branch it will merge into changes, Freeside repeats the review and any
checks affected by the change. Your decisions survive retries and restarts,
but an approval applies only to the versions you approved. If its inputs
change, it no longer applies.
Read the [introduction](docs/intro.md) for the core ideas and the intended
experience.

**Freeside is still under development. Expect rough edges.** The pinned wave
tracker in [GitHub Issues](https://github.com/freeside-ai/freeside/issues)
records implementation progress.

| I Want To… | Start Here | What To Expect |
| --- | --- | --- |
| Explore the interface | [Try the app](#try-the-app-with-sample-data) | Sample decisions in memory, with no daemon or agent accounts. |
| Install a local client and daemon | [Install on Mac](docs/getting-started.md#install-on-mac) | A signed app, background service, and device pairing. Agent execution stays disabled until configured. |
| Run agents on a repository | [Prepare a real run](docs/getting-started.md#prepare-a-real-repository-run) | GitHub App setup, agent credentials, container images, and explicit execution policy. |
| Use an iPhone as a client | [Connect an iPhone](docs/getting-started.md#connect-an-iphone) | A source-built app paired with a reachable daemon on your Mac. |

## Try the App With Sample Data

This is the shortest path to seeing how Freeside works. It needs Xcode, but no
Apple signing identity, Go, daemon, GitHub App, or agent credentials.

```sh
xcodebuild -project app/Freeside.xcodeproj -scheme FreesideMac \
  -destination 'platform=macOS' -skipPackagePluginValidation \
  CODE_SIGNING_ALLOWED=NO -derivedDataPath /tmp/freeside-preview build
open -n /tmp/freeside-preview/Build/Products/Debug/FreesideMac.app \
  --args -FreesideMock YES
```

The app opens with sample attention items. Select a card to read its context,
inspect details, and try its offered actions. These actions change mock state
only; they do not run agents, edit repositories, or publish pull requests.
Quit the preview before opening the installed app below.

## Development and Further Reading

- **Project goals:** [Introduction](docs/intro.md).
- **Architecture and roadmap:** [Project plan](docs/plan.md). Live phase and
  wave status comes from the open wave tracker (`Wave N: <Name>`, labeled
  `tracker` with a milestone) in
  [GitHub Issues](https://github.com/freeside-ai/freeside/issues), using the
  plan's [wave-tracker resolution rule](docs/plan.md#implementation-coordination-building-freeside-with-agents).
- **Component guides:** [App](app/README.md), [daemon](daemon/README.md),
  [API](api/README.md), and [images](images/README.md).
- **Contributing:** [CONTRIBUTING.md](CONTRIBUTING.md) and [repository conventions](AGENTS.md).
  Run `bash scripts/check.sh --list` for available checks, then
  `bash scripts/check.sh <component>` for the component you changed.

## License

This work is licensed under [AGPL-3.0-or-later](./LICENSE).

The copyright holder applies this grant to every revision in this repository's
history, except material that states a different license or copyright holder.

See [LICENSING-PHILOSOPHY.md](./LICENSING-PHILOSOPHY.md) for why we chose
this license.

The philosophy describes how we choose a license for a project; it does not
grant separate licenses to individual files in this repository.

---

A [Free as in Bird](https://freeasinbird.com) project.
