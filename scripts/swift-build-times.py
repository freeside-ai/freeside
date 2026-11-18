#!/usr/bin/env python3
"""Record each tracked source file's modification time and content hash, and
give unchanged files their recorded time back.

A fresh checkout gives every file a new modification time, and SwiftPM
treats a file whose time differs from the one its build saw as changed, so a
restored app/.build that is not an exact match recompiles every module
(#3294). app-ci.yml's `swift-build-cache` job runs `record` after the build,
so the saved entry carries the time each file had when it was built.
convergence-ci.yml's `convergence` job runs `apply` after it restores an
entry that is not an exact match (#3295), so each file whose hash still
matches gets the time the build saw, and the build that follows recompiles
only what changed.

Usage: swift-build-times.py record <tree> <out>
       swift-build-times.py apply <tree> <record>

Rules for record:
- The files are the ones `git -C <tree> ls-files` lists, as paths relative to
  <tree>. Untracked and ignored files, including app/.build, are left out.
- Every listed path must be a regular file it can read. A missing, unreadable,
  or non-regular one (a symlink, a directory, a submodule) fails the run,
  naming the path, because a record that skips a file would leave the reader
  to rebuild it without saying why. So does a path holding a tab or newline,
  which the format cannot carry.
- The time is st_mtime_ns from lstat, taken before the file is read.
- <out> is written whole or not at all: a temporary file beside it is renamed
  into place only after every file has been recorded.

Rules for apply:
- It reads and checks the whole record before it touches a file. A missing or
  unreadable record, a wrong header, a line that is not
  `<digits>\t<64 lowercase hex>\t<path>`, or a path recorded twice means
  there is no usable record: it changes no time, says why, and exits 3.
- The files are the ones `git -C <tree> ls-files` lists, so a record can only
  touch tracked files under <tree>. A listed regular file whose SHA-256
  equals its recorded hash gets the recorded modification time; its access
  time is kept. A file whose content differs, one with no line in the
  record, and one that is not a regular file keep the time they have, so the
  build sees them as changed. A recorded path git no longer lists is
  skipped.
- A git failure or a file it cannot read or retime fails the run, possibly
  after some times were applied; a time applied this way is right for its
  file, so the build stays correct either way.

Format, written by record and read by apply:
    # freeside swift-build-times v1
    <mtime in ns>\t<sha256 hex>\t<path relative to <tree>>
one line per file, sorted by path bytes.

Output goes to stderr: one line with the counts on success, the failing
path or the reason the record is unusable otherwise.

Exit codes: 0 recorded or applied; 1 a file or git failure; 2 usage error;
3 apply found no usable record and changed nothing.
"""

import argparse
import hashlib
import os
import re
import stat
import subprocess
import sys
import tempfile

HEADER = b"# freeside swift-build-times v1\n"
RECORD_LINE = re.compile(rb"([0-9]+)\t([0-9a-f]{64})\t([^\t]+)")


class RecordError(Exception):
    pass


class UnusableRecord(Exception):
    """The record can't be used, so apply changes nothing (exit 3)."""


def tracked_paths(tree):
    """Returns the tracked paths under tree as bytes, sorted by byte value."""
    try:
        listed = subprocess.run(["git", "-C", tree, "ls-files", "-z"],
                                check=True, capture_output=True).stdout
    except subprocess.CalledProcessError as err:
        raise RecordError(f"git ls-files in {tree} failed: "
                          f"{err.stderr.decode(errors='replace').strip()}") from err
    return sorted(path for path in listed.split(b"\0") if path)


def file_digest(full):
    digest = hashlib.sha256()
    with open(full, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def record_line(tree, path):
    shown = path.decode(errors="backslashreplace")
    if b"\t" in path or b"\n" in path:
        raise RecordError(f"{shown!r}: a tab or newline in the path")
    full = os.path.join(os.fsencode(tree), path)
    try:
        st = os.lstat(full)
        if not stat.S_ISREG(st.st_mode):
            raise RecordError(f"{shown}: not a regular file")
        digest = file_digest(full)
    except OSError as err:
        raise RecordError(f"{shown}: {err.strerror}") from err
    return b"%d\t%s\t%s\n" % (st.st_mtime_ns, digest.encode(), path)


def record(tree, out):
    lines = [record_line(tree, path) for path in tracked_paths(tree)]
    directory = os.path.dirname(os.path.abspath(out))
    fd, temp = tempfile.mkstemp(dir=directory, prefix=".swift-build-times.")
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(HEADER)
            handle.writelines(lines)
        os.replace(temp, out)
    except BaseException:
        os.unlink(temp)
        raise
    return len(lines)


def read_record(record_path):
    """Returns {path: (mtime_ns, sha256 hex)} from a whole, valid record."""
    try:
        with open(record_path, "rb") as handle:
            data = handle.read()
    except OSError as err:
        raise UnusableRecord(f"{record_path}: {err.strerror}") from err
    if not data.startswith(HEADER):
        raise UnusableRecord(f"{record_path}: not a v1 record (wrong header)")
    body = data[len(HEADER):]
    if body and not body.endswith(b"\n"):
        raise UnusableRecord(f"{record_path}: the last line is cut off")
    entries = {}
    # Split on newlines only: a path may hold a carriage return.
    for number, line in enumerate(body.split(b"\n")[:-1], start=2):
        match = RECORD_LINE.fullmatch(line)
        if match is None:
            raise UnusableRecord(f"{record_path}: line {number} is malformed")
        mtime, digest, path = match.groups()
        if path in entries:
            raise UnusableRecord(f"{record_path}: line {number} repeats "
                                 f"{path.decode(errors='backslashreplace')}")
        entries[path] = (int(mtime), digest.decode())
    return entries


def apply(tree, record_path):
    """Gives each unchanged tracked file its recorded time; returns the counts."""
    entries = read_record(record_path)
    paths = tracked_paths(tree)
    counts = {"applied": 0, "changed": 0, "unrecorded": 0}
    for path in paths:
        entry = entries.get(path)
        if entry is None:
            counts["unrecorded"] += 1
            continue
        mtime, recorded = entry
        full = os.path.join(os.fsencode(tree), path)
        try:
            st = os.lstat(full)
            if not stat.S_ISREG(st.st_mode) or file_digest(full) != recorded:
                counts["changed"] += 1
                continue
            os.utime(full, ns=(st.st_atime_ns, mtime), follow_symlinks=False)
        except OSError as err:
            raise RecordError(f"{path.decode(errors='backslashreplace')}: {err.strerror}") from err
        counts["applied"] += 1
    counts["tracked"] = len(paths)
    counts["gone"] = len(entries.keys() - set(paths))
    return counts


def main(argv):
    parser = argparse.ArgumentParser(description="Record and apply tracked source file times.")
    commands = parser.add_subparsers(dest="command", required=True)
    record_parser = commands.add_parser("record", help="write the record of <tree> to <out>")
    record_parser.add_argument("tree")
    record_parser.add_argument("out")
    apply_parser = commands.add_parser(
        "apply", help="give the unchanged tracked files of <tree> their times in <record>")
    apply_parser.add_argument("tree")
    apply_parser.add_argument("record")
    args = parser.parse_args(argv)

    if args.command == "record":
        count = record(args.tree, args.out)
        print(f"swift build times: recorded {count} tracked files under {args.tree} in {args.out}",
              file=sys.stderr)
        return 0
    try:
        counts = apply(args.tree, args.record)
    except UnusableRecord as err:
        print(f"swift build times: no usable record, applied nothing: {err}", file=sys.stderr)
        return 3
    print(f"swift build times: applied recorded times to {counts['applied']} of "
          f"{counts['tracked']} tracked files under {args.tree}; {counts['changed']} changed, "
          f"{counts['unrecorded']} not in the record, {counts['gone']} recorded files no longer "
          "tracked", file=sys.stderr)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except (RecordError, OSError) as err:
        print(f"swift build times: {err}", file=sys.stderr)
        sys.exit(1)
