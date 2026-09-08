# Freeside

**Freeside runs coding-agent workflows locally, turning tasks into
evidence-backed pull requests and asking you when a decision is needed.**
It controls which work starts, where agents can work, which credentials they
receive, and what verification must pass. A background service (the daemon)
saves workflow state so it survives restarts. Mac and iPhone apps show
progress and decisions. The harness runs the agent; you hold the reins.
Read the [introduction](docs/intro.md) for the core ideas.

**Freeside is still under development.** You currently build it from source
to install it. You can preview the interface, install the Mac client and local
daemon, or prepare a real repository run. Running real agents still needs
substantial manual setup; the local installer alone does not enable it.

| I Want To… | Start Here | What To Expect |
| --- | --- | --- |
| Explore the interface | [Try the app](#try-the-app-with-sample-data) | Sample decisions in memory, with no daemon or agent accounts. |
| Install a local client and daemon | [Install on Mac](#install-on-mac) | A signed app, background service, and device pairing. Execution is simulated by default. |
| Run agents on a repository | [Prepare a real run](#prepare-a-real-repository-run) | GitHub App setup, agent credentials, container images, and explicit execution policy. |
| Use an iPhone as a client | [Connect an iPhone](#connect-an-iphone) | A source-built app paired with a reachable daemon on your Mac. |

## Get the Source

The Mac paths below use **Xcode 26.6**, the toolchain used by this repository's
CI. Install full Xcode and complete its first-launch setup; Command Line Tools
alone cannot build the app. Building the daemon also needs **Go 1.26.6**, as
declared in [daemon/go.mod](daemon/go.mod). The daemon core builds on Linux,
but the installation and real execution paths here are Mac-first.

```sh
git clone https://github.com/freeside-ai/freeside.git
cd freeside
xcodebuild -version
```

Run all commands below from this repository root unless stated otherwise.
Build the client and daemon from the same checkout: their API changes together.

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

## Install on Mac

In **Xcode > Settings > Accounts**, add your Apple ID and set up an
**Apple Development** signing identity for your personal team. The installer
uses it to give the app private Keychain storage for its device credentials.
The installed client cannot use ad-hoc signing. A free personal team is
sufficient.

Build the daemon, then install the app with that binary bundled inside it:

```sh
go -C daemon build -o /tmp/freesided ./cmd/freesided
bash app/scripts/install-mac-app.sh \
  --daemon-path /tmp/freesided \
  --server-url http://127.0.0.1:7331 \
  --launch
```

The CLI binary is at `/tmp/freesided`; the installer does not add it to your
`PATH`. Use that full path wherever the operator guides below say `freesided`.

The installer places the app at `~/Applications/Freeside.app`. On launch, the
app registers its bundled daemon as a user LaunchAgent. No root installation
is needed. If macOS requests background-service approval, use **Open Login
Items…** from the app and approve Freeside.

### Pair and Check the Connection

1. Open the Freeside menu bar item and check the daemon's status. Use **Start**
   if it is stopped.
2. The app reads the local daemon's readiness file and prefills its pairing
   code. Check the displayed host details and choose **Pair this device**.
3. Open **Inbox** and **Runs**. On a fresh installation, an empty inbox is
   expected. The bundled service uses a fake driver, which simulates execution,
   and starts without demo tasks. Use the sample-data preview above for a
   populated interface.

To check that the default local daemon is responding:

```sh
curl --fail --silent --show-error http://127.0.0.1:7331/health
```

A healthy response confirms the daemon is reachable. It does not tell you
whether a repository or agent is ready for real work.

### Update and Stop

After updating your checkout, repeat the daemon build and install commands
above. The installer replaces the app and bundled daemon together. Updates
under the same signing identity retain device pairing. Quit Freeside normally
when you are done using the client; use **Stop** in its menu bar controls when
you want to stop the background daemon.

The daemon stores its database and supporting state under
`~/Library/Application Support/Freeside/daemon`. Its diagnostic log is
`freesided.log` in that directory. See the [client installation guide](app/README.md#installing-the-operator-client)
for signing overrides, custom build locations, and update details.

## Use Freeside

A **task** is the work you want done. A **run** is an execution belonging to
that task. An **attention item** is a decision or intervention Freeside needs
from you; it can also concern system health rather than a task.

In a configured real workflow:

1. Submit work through the daemon's `submit` command or a configured intake
   source. The current production exercise packages submission in the script
   described below; installing the client does not configure intake.
2. Read the generated specification in **Inbox**. Approve it or request
   changes before implementation begins.
3. Follow progress in **Runs**. Return to **Inbox** for agent questions,
   execution failures, or review decisions. Read the card's evidence and use
   the actions it offers.
4. Review the resulting pull request and its verification evidence before
   deciding whether to merge.

On Mac, **⌘1** opens Inbox, **⌘2** opens Runs, **⌘R** refreshes, and **⌥⌘I**
toggles the inspector. The app shows its last-updated state; refresh and check
the connection if the information looks stale. The [client guide](app/README.md)
describes keyboard commands and connection modes in more detail.

## Prepare a Real Repository Run

The repository includes a script for running and checking a real workflow,
called the production exercise. You still need to prepare its inputs by hand.
The installed daemon's default fake driver does not launch coding agents.
Start with a small repository and a narrowly scoped task whose expected change
you can review.

The real-run script requires a clean Freeside checkout. Xcode builds, including
the preview and app installation above, can rewrite the tracked dependency file
shown below. Check for changes before proceeding:

```sh
git status --short
git diff -- app/Freeside.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved
```

If that file's changes came only from the build and you do not want to keep
them, restore that file from Git. Preserve any intentional edits. If you are
unsure which changes came from the build, use a separate clean checkout for
the real run. Run `git status --short` again; it should print nothing.

Prepare these pieces in order, using the linked instructions:

1. **Permission to publish on GitHub.** Follow [GitHub App credential onboarding](daemon/README.md#github-app-credential-onboarding)
   and [operational commands](daemon/README.md#operational-commands).
   `freesided setup -operator <login> -operator-id <id>` starts registration;
   the default configuration root is `~/.freeside`. This is separate from the
   installed daemon's state directory. Setup configures GitHub permissions;
   it does not install the Mac app. Keep registration codes and private keys
   out of command arguments and use the documented standard-input flow.
2. **Execution environment.** Prepare Apple `container`, the pinned
   [agent and exporter images](images/README.md), authenticated agent
   identities, and a Codex reviewer identity. The current production exercise
   uses Claude for execution and Codex for review. It also needs an exact
   repository base, allowed paths, trusted prompt packages, and an approved
   verification recipe.
3. **Repository and verification.** `freesided onboard <owner/name>` resolves
   the App installation, audits the repository, asks you to approve its trust
   profile (the rules for working on that repository), and builds its project
   image from the prepared base image. The
   [daemon guide](daemon/README.md#operational-commands) explains the required
   inputs and approval sequence; the bare command name here is not a complete
   onboarding invocation.
4. **Submission and supervision.** Read the required environment and input
   file formats in [scripts/run-real-work.sh](scripts/run-real-work.sh), then
   follow the [production walkthrough and recovery guide](docs/production-walkthrough.md).
   The invocation has this form after those inputs are prepared:

   ```sh
   bash scripts/run-real-work.sh \
     /path/to/task.md /path/to/resolved-policy-keys.json /path/to/publication.json
   ```

Keep a paired client open to approve or revise the specification. This path
runs real agents and can publish a pull request to the configured repository.
The script retains the running session after verification; finish it using
the exact completion command it prints, following the runbook's restoration
steps. Closing its terminal is an interruption, not normal completion.

There is not yet a single guided flow that assembles all these inputs. The
roadmap's [operations and onboarding section](docs/plan.md#10-operations-and-onboarding)
describes the intended experience; use the implementation guides above for
what you can run today.

## Connect an iPhone

The iPhone is an optional client; the daemon and execution stay on the Mac.
The source installer uses your free personal team. You need a connected,
unlocked, trusted iPhone running iOS 17 or later with Developer Mode enabled.

Follow [Installing on an iOS Device](app/README.md#installing-on-an-ios-device)
for signing, first-launch trust, installation commands, and pairing. For a real
connection, both devices must be on the same Tailscale tailnet, and the daemon
must listen on the Mac's Tailscale address. The default Mac installation
listens on loopback only, so it is not reachable from the phone as installed.
Use the documented numeric Tailscale IP as the client endpoint. Personal-team
device builds need periodic re-signing; the guide covers that maintenance.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Xcode build tools are missing or too old | Check `xcodebuild -version` and select the full Xcode installation used for this checkout. |
| The installer cannot choose a signing identity | Check Xcode Accounts and the [signing identity override](app/README.md#installing-the-operator-client), especially if you have multiple teams. |
| The daemon is stopped or unreachable | Check the menu bar status, Login Items approval, the health URL, and `freesided.log`. After a production exercise, follow the [restoration steps](docs/production-walkthrough.md#restore-the-supervised-daemon). |
| Pairing succeeds but sync stays stale | Rebuild and reinstall the client and daemon from the same commit; an API mismatch can appear as a freshness failure. |
| A new installation has no tasks | This is expected for the default service. Preview sample data above, or complete real-run setup before submitting work. |

## Development and Further Reading

- **Project goals:** [Introduction](docs/intro.md).
- **Architecture and roadmap:** [Project plan](docs/plan.md). Live phase and
  wave status comes from the pinned `Wave N (...) tracking` issue in
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
