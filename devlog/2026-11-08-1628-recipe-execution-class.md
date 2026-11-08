# Name Each Recipe Command's Execution Class

Work unit: #2875, which implements the plan §5.7 commitment that #2874
recorded: each command of a trusted recipe names the room that runs it.
Mandatory note: a `kind:contract` change to the trusted recipe and the
verification report, and Tier A work in `internal/verify/`, the image
builder's offline proof, and the engine's verification options.

## Decision: A Command Is an Argv Array or an Object

A recipe command is either the bare argv array it always was, which runs
in the project image (`project_image`), or an object
`{"argv": [...], "room": "macos_host"}` whose `argv` and `room` are both
required. Chose that over a parallel `rooms` array beside `commands`, or
a required room on every command, because the digest is a hash of the
recipe's exact bytes and both alternatives change the bytes of every
recipe already approved. With the two forms, every existing recipe keeps
its bytes, its digest, and its approval.

The object form defaults nothing: a missing or null `room` is refused
rather than read as `project_image`, so a command written in the object
form always says where it runs. Each command decodes in a second
`strictjson` pass over its raw bytes, so the object form keeps the outer
decode's refusal of unknown fields and duplicate names. A command's
class is part of the bytes the digest covers, so moving a command to
another room needs reapproval.

## Decision: A Missing Room Is a Typed Error, Not a Finding

`verify.Options` takes a room per class. A recipe that names a class the
caller supplied no room for fails with `ErrRoomUnavailable`, naming the
class, before any workspace is materialized or any room runs. Chose that
over a finding or a failed `Result`, because the verification never ran:
a finding or a failed outcome would emit evidence for a check nobody
performed, and the caller chose the rooms, so the miss is the caller's
integrity failure. The command never falls back to another class's room.
#2882, which selects the rooms in the engine, maps the error into a
failed verification that blocks and recovers like any other.

Until then the engine supplies only the project-image room, yet
onboarding already accepts a recipe with a `macos_host` command. So the
engine classes `ErrRoomUnavailable` with the other verify refusals that
contradict Freeside's own state, not as a retryable error: a retry meets
the same missing room, and retrying would loop on it forever. A
fresh-context review found that path before push.

## Decision: The Report Records the Class, Omitted When Absent

Each report step carries `room`, the class whose room ran it, written by
the verifier on every new report. The field is `omitempty`, and
`ParseReport` reads a step without it as `project_image`, so a report
stored before this unit still parses byte-for-byte and keeps its
artifact digest; the `evaluated_sha` field set that precedent.
`ParseReport` refuses an unknown room.

## Decision: The Image Proves Only Its Own Commands

The project-image builder's offline proof runs only the recipe's
`project_image` commands, because the image never runs a `macos_host`
command and a macOS build cannot run in it. A recipe with no
`project_image` command is legal and proves no recipe command in the
image; nothing in this unit refuses it.

Revisit when a third execution class is proposed, or when the owner wants
the image proof to refuse a recipe that gives the image nothing to prove.
