# No Process the Daemon Starts Carries a Secret in Its Environment

Issue #3550 makes plan §5.7's precondition for cordoned runs true: no
process the daemon starts carries a secret in its argv or environment. No
rule hides one process's environment from another on the same account, and
the cordoned-writer spike read a CLI's token through `KERN_PROCARGS2`. Three
crossings did it: the host judgment CLI's `CLAUDE_CODE_OAUTH_TOKEN`, and the
`GIT_CONFIG_VALUE_0` header in the publish transport and the project-image
clone. The owner handed over the issue's contract and implementation plan
with `Handle #3550`; the choices below are the plan's, plus the agent's
implementation choices.

## Chose a Pipe for the Judgment CLI

A call launch and the lineup probe hand the setup token on descriptor 3 and
set `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3`, as the cordoned launch
does. Stdin still carries the request body. The pipe is filled and its
write end closed before the CLI starts, so `credentialPipe` refuses a token
over 16 KiB, which a pipe holds unread; a setup token is at most 4096 bytes.
It is a local copy of the cordon's `secretPipe`: two Tier A packages and two
uses don't earn a shared helper.

The risk was that the CLI honors the descriptor only in the cordoned
writer's mode. The opt-in protocol probe against CLI 2.1.331, now asserting
the fake provider receives the token, passed for every sendable effort.

## Chose a Cordon-Denied Header File for Git

An authenticated git invocation reads its `http.extraHeader` line from a
config file that its `GIT_CONFIG_GLOBAL` names: mode 0600, created per
invocation in a 0700 `git-credentials` directory inside the publication
credentials directory (`daemoncompose.GitCredentialDir`), and removed when
the invocation returns. A failed removal fails the invocation, since the
file still holds a live token.

Rejected:

- **A pipe.** `git` and `git-remote-https` each read their config, so a pipe
  is empty by the second read.
- **The scratch directory under `os.MkdirTemp("")`.** That is the account's
  temporary directory, which the cordon leaves open to the writer.
- **The publication state directory.** The issue's plan named it, and the
  cordon denies it too, but the fresh-context review pointed out it is the
  checkpoint surface no credential may sit in
  (`publish.ErrCredentialsInsideStateDir`, plan §5.10). A crash can leave a
  header file behind, and a later checkpoint would copy it. The
  credentials directory is denied as well and checkpoints exclude it; the
  keystore reads only its own subdirectory there.
- **A credential helper.** It is another program the daemon starts, and it
  must get the token from somewhere; the file is the simplest place the
  cordon already denies.

`GIT_CONFIG_GLOBAL` was already `/dev/null` for every transport invocation;
only the authenticated one now names a file. The `-c` hardening still
outranks every config file, so the file can't loosen redirects, helpers, or
protocols, and it holds only the header.

A scratch check against a local HTTP server, with the spike's `envscan`
looping during the request, showed both halves: the header reached the
server from the file, and the scan found it in no process; with the old
environment triple, it found it in both `git` and `git-remote-http`.

## The File's Safety Rests on the Cordon's Denial

The header file is only as private as the cordon makes it. The cordon denies
`-publication-credentials-dir` to every launch (`cordonDaemonDirs`), and
#3470 proves the denial. Onboarding's clone and preflight's fetch write in
the same credentials directory. `NewTransport` and `projectimage.New` refuse
a credential directory any other user can enter.

The denial covers the instance whose cordon launched the writer. Another
instance's directories are denied only when the operator passes them as
`-cordon-protected-root`, as for the rest of that instance's state. That
isn't a regression: the environment the header used to ride was readable to
every process on the account.

Revisit when the cordoned verification class is built: its rule file must
deny the same directories, or the header file becomes readable to a job. A
crash can leave a header file behind; it holds a token that expires within
an hour, in a denied directory that checkpoints exclude, so no sweep is
added.

## The Platform-Binary Probe Became Moot

The issue first asked whether `/usr/bin/git` hides its environment as a
platform binary. It no longer matters: the token is in no environment, and
`git` hands its environment to `git-remote-https`, which isn't a platform
binary.

## Refute-First Findings

One fresh-context reviewer read the diff to refute it. Outcomes:

- **Confirmed, fixed: header files on the checkpoint surface.** The files
  moved from the publication state directory to the credentials directory,
  above.
- **Confirmed, recorded: the denial covers one instance.** See the section
  above; the wording in the plan and the package docs names the denied
  directory rather than claiming more.
- **Disproved by a check: the CLI writes the descriptor's token to disk.**
  CLI 2.1.331 persists a descriptor token only when `CLAUDE_CODE_REMOTE` is
  set, and `commandEnv` never sets it.
- **Disproved by a check: a 16 KiB token blocks the pipe write.** A fresh
  pipe with no reader on this Mac takes 64 KiB.
- **Disproved by a trace: a successful push whose cleanup fails is lost.**
  The push reports failure, and the retry sees the ref at the head SHA and
  converges.
- **Disproved by a trace: the deferred removal in `gitSource.Fetch` misses
  the result.** Nothing in its block shadows the named `err`.
