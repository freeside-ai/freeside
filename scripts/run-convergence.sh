#!/usr/bin/env bash
# run-convergence.sh — the §5.14 real-daemon convergence pass (issue #72).
#
# Usage: run-convergence.sh
#
# Builds the freeside-signet-dev harness from daemon/, launches it on
# loopback with a temporary state directory, reads the readiness line
# for the two bound URLs, and runs the FreesideConvergenceTests suite
# against it. Without those URLs in the environment the suite skips,
# so this script is the one entry point that actually exercises the
# client halves of the sixteen-test matrix against a real daemon.
#
# Requires: Go (daemon toolchain), Swift (app toolchain), macOS.
set -euo pipefail

# The convergence target exists only on Darwin (app/Package.swift), and
# SwiftPM's --filter exits 0 when nothing matches, so off macOS this
# script would report success having run zero tests.
if [[ "$(uname)" != "Darwin" ]]; then
  echo "run-convergence: requires macOS (FreesideConvergenceTests is Darwin-only)" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workdir="$(mktemp -d)"
harness_pid=""

cleanup() {
  if [[ -n "$harness_pid" ]] && kill -0 "$harness_pid" 2>/dev/null; then
    kill "$harness_pid" 2>/dev/null || true
    wait "$harness_pid" 2>/dev/null || true
  fi
  rm -rf "$workdir"
}
trap cleanup EXIT

echo "building freeside-signet-dev" >&2
(cd "$repo_root/daemon" && go build -o "$workdir/freeside-signet-dev" ./cmd/freeside-signet-dev)

"$workdir/freeside-signet-dev" -db "$workdir/signet.db" > "$workdir/readiness.json" &
harness_pid=$!

# The harness prints one JSON readiness line on stdout once both
# listeners are bound; give it a bounded window. Wait for the trailing
# newline, not just first bytes, so a partially flushed line never
# reaches the extraction below.
ready=""
for _ in $(seq 1 50); do
  if [[ -s "$workdir/readiness.json" ]] && grep -q '}' "$workdir/readiness.json"; then
    ready=1
    break
  fi
  if ! kill -0 "$harness_pid" 2>/dev/null; then
    echo "run-convergence: harness exited before readiness" >&2
    exit 1
  fi
  sleep 0.1
done
if [[ -z "$ready" ]]; then
  echo "run-convergence: no readiness line within 5s" >&2
  exit 1
fi

api_url="$(sed -n 's/.*"api_url":"\([^"]*\)".*/\1/p' "$workdir/readiness.json")"
control_url="$(sed -n 's/.*"control_url":"\([^"]*\)".*/\1/p' "$workdir/readiness.json")"
if [[ -z "$api_url" || -z "$control_url" ]]; then
  echo "run-convergence: malformed readiness line: $(cat "$workdir/readiness.json")" >&2
  exit 1
fi
echo "harness ready: api=$api_url control=$control_url" >&2

# A filter that matches nothing, or a suite that skips (env not seen),
# still exits 0, so demand positive evidence that the suite ran and
# passed before this command counts as a convergence pass.
test_log="$workdir/swift-test.log"
(
  cd "$repo_root/app" &&
    FREESIDE_CONVERGENCE_URL="$api_url" \
    FREESIDE_CONVERGENCE_CONTROL_URL="$control_url" \
    swift test --only-use-versions-from-resolved-file --filter FreesideConvergenceTests
) 2>&1 | tee "$test_log"
if ! grep -q "Suite RealDaemonConvergenceTests passed" "$test_log"; then
  echo "run-convergence: the convergence suite did not run to a pass (zero tests matched, or the suite skipped)" >&2
  exit 1
fi
