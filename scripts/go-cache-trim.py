#!/usr/bin/env python3
"""Trim the Go build cache to a size cap, least recently used entries first.

Go caps GOCACHE by age only: it deletes entries unused for 5 days, checked
once a day, and has no size limit (golang/go#29561). Many worktrees at
different commits can fill a disk inside that window, so check.sh runs this
before its daemon steps.

Usage: go-cache-trim.py --cache <dir> --limit-bytes <n> --state <dir>

Rules:
- Only cache entries count or get deleted: a name directly under one of the
  two-hex-digit subdirectories that is 64 lowercase hex digits plus -a or -d
  (Go's DiskCache.fileName). A -d entry can be a directory, which Go 1.26
  uses for cached executables; it counts as the sum of its files and is
  removed whole. Root files (trim.txt, README, testexpire.txt) and any other
  name stay put, and symlinks are neither followed nor touched.
- Size is the sum of st_size, not allocated blocks.
- The walk runs at most once an hour: <state>/last-walk records the last
  completed walk.
- Over the cap, entries go oldest modification time first until the cache
  is at or under 75% of the cap, so the next run doesn't trim again.
- Go refreshes an entry's modification time on use only once it is more than
  an hour old (mtimeInterval), so an entry used in the last hour is less than
  2 hours old. Entries modified in the last 2 hours are never deleted, and
  each entry is stat'ed again just before its delete. When only such entries
  remain over the cap, it warns and exits 0.
- An entry or subdirectory it can't read or delete, such as one a root-owned
  `sudo go` run left, is skipped like Go's own trim skips it, so one such
  entry never stops the rest from going. The result line counts them.
- One trimmer at a time: an exclusive flock on <state>/lock, which the kernel
  releases when the process exits. A second trimmer prints a notice and skips.

Output goes to stderr: nothing when throttled or under the cap, one line
after a trim, and a warning when the cache stays over the cap.

Exit codes: 0 done, skipped, or warned; 1 an unexpected filesystem error;
2 usage error.
"""

import argparse
import fcntl
import os
from pathlib import Path
import re
import shutil
import stat
import sys
import time

ENTRY_NAME = re.compile(r"[0-9a-f]{64}-[ad]")
WALK_INTERVAL = 3600
# Twice Go's mtimeInterval: see the module docstring.
RECENT = 2 * 3600


def positive_int(text):
    value = int(text)
    if value <= 0:
        raise ValueError(text)
    return value


def gb(size):
    return f"{size / 1e9:.1f} GB"


def entry_size(path, st):
    """Returns a file entry's size, or the summed size of a directory entry's files."""
    if stat.S_ISREG(st.st_mode):
        return st.st_size
    total = 0
    for dirpath, _dirs, files in os.walk(path):
        for name in files:
            try:
                total += os.lstat(os.path.join(dirpath, name)).st_size
            except OSError:
                pass
    return total


def walk(cache):
    """Returns [(mtime, size, path, is_dir)] for every cache entry."""
    entries = []
    for prefix in range(256):
        subdir = os.path.join(cache, f"{prefix:02x}")
        try:
            if not stat.S_ISDIR(os.lstat(subdir).st_mode):
                continue
            listing = list(os.scandir(subdir))
        except OSError:
            continue
        for item in listing:
            if not ENTRY_NAME.fullmatch(item.name):
                continue
            try:
                st = item.stat(follow_symlinks=False)
                if not (stat.S_ISREG(st.st_mode) or stat.S_ISDIR(st.st_mode)):
                    continue
                entries.append((st.st_mtime, entry_size(item.path, st), item.path,
                                stat.S_ISDIR(st.st_mode)))
            except OSError:
                continue
    return entries


def trim(limit, entries, now):
    """Deletes old entries until the total is at or under 75% of limit.

    Returns (size after, entries removed, entries that failed to delete). A
    failed entry still counts toward the size, even if it went in part."""
    total = sum(size for _, size, _, _ in entries)
    target = limit * 3 // 4
    removed = failed = 0
    for mtime, size, path, is_dir in sorted(entries):
        if total <= target or now - mtime < RECENT:
            break
        # A build may have used the entry since the walk.
        try:
            st = os.lstat(path)
        except FileNotFoundError:
            total -= size
            continue
        except OSError:
            failed += 1
            continue
        if stat.S_ISDIR(st.st_mode) != is_dir or stat.S_ISLNK(st.st_mode):
            continue
        if time.time() - st.st_mtime < RECENT:
            continue
        try:
            if is_dir:
                shutil.rmtree(path)
            else:
                os.remove(path)
        except FileNotFoundError:
            pass
        except OSError:
            failed += 1
            continue
        total -= size
        removed += 1
    return total, removed, failed


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--cache", required=True)
    parser.add_argument("--limit-bytes", required=True, type=positive_int)
    parser.add_argument("--state", required=True)
    args = parser.parse_args(argv)

    state = Path(args.state)
    state.mkdir(parents=True, exist_ok=True)
    lock_path = state / "lock"
    # Held until exit; the kernel releases it even on SIGKILL.
    lock = open(lock_path, "a")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        print(f"go cache trim: another trim holds {lock_path}; skipping", file=sys.stderr)
        return 0

    stamp = state / "last-walk"
    now = time.time()
    try:
        age = now - stamp.stat().st_mtime
        if 0 <= age < WALK_INTERVAL:
            return 0
    except FileNotFoundError:
        pass

    entries = walk(args.cache)
    stamp.touch()
    before = sum(size for _, size, _, _ in entries)
    if before <= args.limit_bytes:
        return 0
    after, removed, failed = trim(args.limit_bytes, entries, now)
    if removed or failed:
        unremovable = f"; could not remove {failed}" if failed else ""
        print(f"go cache trim: {args.cache} was {gb(before)}, now {gb(after)} "
              f"(cap {gb(args.limit_bytes)}); removed {removed} entries{unremovable}",
              file=sys.stderr)
    if after > args.limit_bytes:
        print(f"go cache trim: warning: {args.cache} holds {gb(after)}, over the "
              f"{gb(args.limit_bytes)} cap; the rest were modified in the last 2 hours"
              f"{' or could not be removed' if failed else ''}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except OSError as err:
        print(f"go cache trim: {err}", file=sys.stderr)
        sys.exit(1)
