#!/usr/bin/env bash
# Manually exercise the reusable project-image builder (issue #334).
#
# The Go package owns repository materialization, build context generation,
# offline proof, registry seeding, and the durable result. A local-registry
# build retains that managed registry to keep its returned reference runnable.
# This wrapper only
# supplies the repository-local ward image checker; `freesided onboard` later
# imports the same package instead of invoking this script.
#
# Usage:
#   scripts/build-project-image.sh -db PATH -repository OWNER/NAME \
#     -repository-id ID -commit SHA -recipe PATH \
#     -base-image NAME@sha256:DIGEST -base-build-ref LOCAL_TAG \
#     (-registry HOST[/PATH] | -local-registry-port PORT) \
#     [-build-proxy http://HOST:PORT]
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root/daemon"
exec go run ./cmd/freeside-project-image \
	"$@"
