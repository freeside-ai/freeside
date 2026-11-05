# Getting Started

This guide takes you from a fresh checkout to a real run: install the Mac
app and its daemon, pair a device, submit work, and connect an iPhone. For a
first look without any of that, build the app with sample data as the
[README](../README.md) describes.

The paths here are Mac-first. The daemon core builds on Linux, but the
installation and real execution paths below are Mac-only today.

## What You Need

- **Xcode 27.2**, the toolchain this repository's Swift tests use. Install
  full Xcode and complete its first-launch setup; Command Line Tools alone
  can't build the app.
- **Go 1.26.10**, as [daemon/go.mod](../daemon/go.mod) declares, to build the
  daemon.

```sh
git clone https://github.com/freeside-ai/freeside.git
cd freeside
xcodebuild -version
```

Run every command in this guide from the repository root unless it says
otherwise. Build the client and daemon from the same checkout: their API
changes together.

## Install on Mac

If the sample-data preview is open, quit it before you open the installed
app.

In **Xcode > Settings > Accounts**, add your Apple ID and set up an
**Apple Development** signing identity for your personal team. The installer
uses it to give the app private Keychain storage for its device credentials.
The installed client can't use ad-hoc signing. A free personal team is
enough.

Build the daemon, then install the app with that binary bundled inside it:

```sh
go -C daemon build -o /tmp/freesided ./cmd/freesided
bash app/scripts/install-mac-app.sh \
  --daemon-path /tmp/freesided \
  --server-url http://127.0.0.1:7331 \
  --launch
```

The CLI binary is at `/tmp/freesided`; the installer doesn't add it to your
`PATH`. Use that full path wherever the operator guides below say
`freesided`.

The installer places the app at `~/Applications/Freeside.app`. On launch, the
app registers its bundled daemon as a user LaunchAgent. No root installation
is needed. If macOS asks you to approve a background service, use **Open
Login Items…** from the app and approve Freeside.

### Pair and Check the Connection

1. Open the Freeside menu bar item and check the daemon's status. Use
   **Start** if it is stopped.
2. The app reads the local daemon's readiness file and prefills its pairing
   code. Check the displayed host details and choose **Pair this device**.
3. Open **Inbox** and **Tasks**. A fresh installation has no tasks and shows
   a setup notice explaining that agent execution is not configured. The
   daemon serves pairing, stored state, and backups without running agents
   or simulating work. Follow [Prepare a Real Repository Run](#prepare-a-real-repository-run)
   when you are ready.

To check that the default local daemon is responding:

```sh
curl --fail --silent --show-error http://127.0.0.1:7331/health
```

A healthy response confirms the daemon is reachable. It doesn't tell you
whether a repository or agent is ready for real work.

### Update and Stop

After updating your checkout, repeat the daemon build and install commands
above. The installer replaces the app and bundled daemon together. Updates
under the same signing identity keep your device pairing. Quit Freeside
normally when you're done using the client; use **Stop** in its menu bar
controls when you want to stop the background daemon.

The daemon stores its database and supporting state under
`~/Library/Application Support/Freeside/daemon`. Its diagnostic log is
`freesided.log` in that directory. See the
[client installation guide](../app/README.md#installing-the-operator-client)
for signing overrides, custom build locations, and update details.

## Use Freeside

A **task** is the work you want done. A **run** is an execution belonging to
that task. An **attention item** is a decision or intervention Freeside needs
from you; it can also concern system health rather than a task.

In a configured real workflow:

1. Submit work through the daemon's `submit` command or a configured intake
   source. A real run submits with `freesided submit` against the deputy
   described below; installing the client does not configure intake.
2. Read the generated specification in **Inbox**. Approve it or request
   changes before implementation begins.
3. Follow progress in **Tasks**; a task's timeline lists its runs. Return to
   **Inbox** for agent questions, execution failures, or review decisions.
   Read the card's evidence and use the actions it offers.
4. Review the resulting pull request and its verification evidence before
   deciding whether to merge.

On Mac, **⌘1** opens Inbox, **⌘2** opens Tasks, and **⌘R** refreshes. The app
shows its last-updated state; refresh and check the connection if the
information looks stale. The [client guide](../app/README.md) describes
keyboard commands and connection modes in more detail.

## Prepare a Real Repository Run

There is not yet a single guided flow that assembles everything a real run
needs. The roadmap's
[operations and onboarding section](plan.md#10-operations-and-onboarding)
describes the intended experience; this section is what you can run today.

A real workflow, called the production exercise, runs under
`freesided deputy`: a separate daemon that executes the run while the
installed daemon keeps running without a driver. You prepare its inputs by
hand. Start with a small repository and a narrowly scoped task whose expected
change you can review.

The real run needs a clean Freeside checkout. Run `git status --short`; it
should print nothing. If it lists
`app/Freeside.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved`,
see [Troubleshooting](#troubleshooting).

Prepare these pieces in order, using the linked instructions:

1. **Permission to publish on GitHub.** Follow
   [GitHub App credential onboarding](../daemon/README.md#github-app-credential-onboarding)
   and [operational commands](../daemon/README.md#operational-commands).
   `freesided setup -operator <login> -operator-id <id>` starts registration;
   the default configuration root is `~/.freeside`. This is separate from the
   installed daemon's state directory. Setup configures GitHub permissions;
   it does not install the Mac app. Keep registration codes and private keys
   out of command arguments and use the documented standard-input flow.
2. **Execution environment.** Prepare Apple `container`, the pinned
   [agent and exporter images](../images/README.md), authenticated agent
   identities, and a Codex reviewer identity. The current production exercise
   uses Claude for execution and Codex for review. It also needs an exact
   repository base, allowed paths, trusted prompt packages, and an approved
   verification recipe.
3. **Repository and verification.** `freesided onboard <owner/name>` resolves
   the App installation, audits the repository, asks you to approve its trust
   profile (the rules for working on that repository), and builds its project
   image from the prepared base image. The
   [daemon guide](../daemon/README.md#operational-commands) explains the
   required inputs and approval sequence; the bare command name here is not a
   complete onboarding invocation.
4. **Submission and supervision.** Write the deputy's settings file, as
   [Run Real Work With The Deputy](../daemon/README.md#run-real-work-with-the-deputy)
   describes, then follow the
   [production walkthrough and recovery guide](production-walkthrough.md).
   Start the run, then submit the task from a second terminal:

   ```sh
   freesided deputy -root /path/to/root
   freesided submit -db /path/to/root/daemon/freeside.db --require-composition \
     -project <project> -task /path/to/task.md -policy /path/to/resolved-policy-keys.json \
     -publication /path/to/publication.json -work-unit /path/to/work-unit.json
   ```

Keep a paired client open to approve or revise the specification. This path
runs real agents and can publish a pull request to the configured repository.
The deputy keeps serving after publication so the paired clients can work
with the result. `freesided inspect task` reads the task's record, the proof
of the run. End the deputy deliberately with Ctrl-C, as the runbook describes;
closing its terminal is an interruption, not normal completion.

## Connect an iPhone

The iPhone is an optional client; the daemon and execution stay on the Mac.
As installed, the Mac daemon listens on loopback only, so the phone can't
reach it. For a real connection, both devices must be on the same Tailscale
tailnet, the daemon must listen on the Mac's Tailscale address, and the
client uses the documented numeric Tailscale IP as its endpoint.

You need a connected, unlocked, trusted iPhone running iOS 17 or later with
Developer Mode enabled. The source installer uses your free personal team.
Follow [Installing on an iOS Device](../app/README.md#installing-on-an-ios-device)
for signing, first-launch trust, installation commands, and pairing. A fresh
app asks for your daemon's address; it never falls back to sample data. After
pairing, it remembers that address for later launches. Personal-team device
builds need periodic re-signing; the guide covers that maintenance.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Xcode build tools are missing or too old | Check `xcodebuild -version` and select the full Xcode installation used for this checkout. |
| The installer can't choose a signing identity | Check Xcode Accounts and the [signing identity override](../app/README.md#installing-the-operator-client), especially if you have multiple teams. |
| The daemon is stopped or unreachable | Check the menu bar status, Login Items approval, the health URL, and `freesided.log`. A real-work run leaves it running; choose **Start** if it was stopped. |
| Pairing succeeds but sync stays stale | Rebuild and reinstall the client and daemon from the same commit; an API mismatch can appear as a freshness failure. |
| A new installation says execution is not configured | Complete real-run setup before submitting work. Sample data and fake execution are available only through explicit demo options. |
| `git status` shows `Package.resolved` changed before a real run | Xcode builds, including the sample-data preview and the app install, can rewrite that tracked file. Compare it with `git diff -- app/Freeside.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved`. If the changes came only from the build and you don't want them, restore the file from Git; keep any intentional edits. Then run `git status --short` again; it should print nothing. If you're unsure which changes came from the build, use a separate clean checkout for the real run. |
