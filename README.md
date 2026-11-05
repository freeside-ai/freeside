# Freeside

Freeside is an agentic development environment (ADE) where you direct coding
agents through a task from its first description to a pull request ready for
your approval.

Tell Freeside what you want to do. It drafts a specification and, once you
approve it, an agent gets to work implementing it on your Mac in the harness
you already use.

You can step away while the work is underway. If a decision needs you,
Freeside sends a card to your phone or Mac. It asks the question directly,
distinguishes the facts from the agent's interpretation, and shows you the
evidence so you can make your own call.

Before the change reaches you as a pull request, Freeside runs the checks and
arranges an independent review. The pull request includes the evidence and
tells you what happened to each finding, whether it was fixed, declined, or
left for later. If the code changes or the target branch moves, Freeside runs
the checks and review again. That way, when you decide whether to merge, you
know the evidence applies to the change in front of you.

**Freeside is still under development. Expect rough edges.** The pinned wave
tracker shows what's landed and what's coming next. You can find it in
[GitHub Issues](https://github.com/freeside-ai/freeside/issues).

## Try It With Sample Data

The quickest way to try Freeside is to open the Mac app with sample data. All
you need is Xcode. From the root of the repository, run:

```sh
xcodebuild -project app/Freeside.xcodeproj -scheme FreesideMac \
  -destination 'platform=macOS' -skipPackagePluginValidation \
  CODE_SIGNING_ALLOWED=NO -derivedDataPath /tmp/freeside-preview build
open -n /tmp/freeside-preview/Build/Products/Debug/FreesideMac.app \
  --args -FreesideMock YES
```

Open a card in the sample inbox to see what needs your attention. Try one of
its actions to see how the inbox responds. Anything you do here affects only
the sample data.

## Getting Started

Once you install the Mac app and pair it with the daemon, Freeside keeps your
state and backups. Your inbox will be empty until you give Freeside real work.

For your first run, pick a small repository and a task you feel comfortable
reviewing yourself. The [getting started guide](docs/getting-started.md)
walks you through the setup, from installing the app and daemon to
configuring a GitHub App for publishing, setting up agent credentials and
container images, and connecting your repository. It tells you what to
expect at each step.

| I want to… | Read |
| --- | --- |
| Install the Mac app and daemon | [Install on Mac](docs/getting-started.md#install-on-mac) |
| Run agents on a repository | [Prepare a Real Repository Run](docs/getting-started.md#prepare-a-real-repository-run) |
| Use an iPhone as a second client | [Connect an iPhone](docs/getting-started.md#connect-an-iphone) |

## Documentation

- [Introduction](docs/intro.md) explains what Freeside is for and the
  thinking behind its design.
- [Plan](docs/plan.md) covers the architecture, decisions, and roadmap. Its
  [wave-tracker rule](docs/plan.md#implementation-coordination-building-freeside-with-agents)
  explains how to find the current wave's tracker.
- The component guides cover the [app](app/README.md),
  [daemon](daemon/README.md), [API](api/README.md), and
  [images](images/README.md).
- To contribute, start with the [contributing guide](CONTRIBUTING.md) and
  [repository conventions](AGENTS.md). Run `bash scripts/check.sh --list` to
  see the available checks, then `bash scripts/check.sh <component>` to check
  the component you changed.

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
