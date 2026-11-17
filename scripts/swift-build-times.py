#!/usr/bin/env python3
"""Record the modification time and content hash of every tracked source file.

A fresh checkout gives every file a new modification time, and SwiftPM
treats a file whose time differs from the one its build saw as changed, so a
restored app/.build that is not an exact match recompiles every module
(#3294). app-ci.yml's `swift-build-cache` job runs `record` after the build,
so the saved entry carries the time each file had when it was built. The
reader (#3295) will restore that time to each file whose hash still
matches.

Usage: swift-build-times.py record <tree> <out>

Rules:
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

Format, the interface #3295 reads:
    # freeside swift-build-times v1
    <mtime in ns>\t<sha256 hex>\t<path relative to <tree>>
one line per file, sorted by path bytes.

Output goes to stderr: one line with the file count on success, the failing
path otherwise.

Exit codes: 0 recorded; 1 a file or git failure; 2 usage error.
"""

import argparse
import hashlib
import os
import stat
import subprocess
import sys
import tempfile

HEADER = b"# freeside swift-build-times v1\n"


class RecordError(Exception):
    pass


def tracked_paths(tree):
    """Returns the tracked paths under tree as bytes, sorted by byte value."""
    try:
        listed = subprocess.run(["git", "-C", tree, "ls-files", "-z"],
                                check=True, capture_output=True).stdout
    except subprocess.CalledProcessError as err:
        raise RecordError(f"git ls-files in {tree} failed: "
                          f"{err.stderr.decode(errors='replace').strip()}") from err
    return sorted(path for path in listed.split(b"\0") if path)


def record_line(tree, path):
    shown = path.decode(errors="backslashreplace")
    if b"\t" in path or b"\n" in path:
        raise RecordError(f"{shown!r}: a tab or newline in the path")
    full = os.path.join(os.fsencode(tree), path)
    try:
        st = os.lstat(full)
        if not stat.S_ISREG(st.st_mode):
            raise RecordError(f"{shown}: not a regular file")
        digest = hashlib.sha256()
        with open(full, "rb") as handle:
            for chunk in iter(lambda: handle.read(1 << 20), b""):
                digest.update(chunk)
    except OSError as err:
        raise RecordError(f"{shown}: {err.strerror}") from err
    return b"%d\t%s\t%s\n" % (st.st_mtime_ns, digest.hexdigest().encode(), path)


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


def main(argv):
    parser = argparse.ArgumentParser(description="Record tracked source file times.")
    commands = parser.add_subparsers(dest="command", required=True)
    record_parser = commands.add_parser("record", help="write the record of <tree> to <out>")
    record_parser.add_argument("tree")
    record_parser.add_argument("out")
    args = parser.parse_args(argv)

    count = record(args.tree, args.out)
    print(f"swift build times: recorded {count} tracked files under {args.tree} in {args.out}",
          file=sys.stderr)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except (RecordError, OSError) as err:
        print(f"swift build times: {err}", file=sys.stderr)
        sys.exit(1)
