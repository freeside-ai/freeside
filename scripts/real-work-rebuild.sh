#!/usr/bin/env bash
# Construct daemon-only rebuild arguments before acquiring any run resources.

real_work_rebuild_args() {
  local base=${FREESIDE_REAL_RUN_BASE_BUILD_REF:-}
  local dns=${FREESIDE_REAL_RUN_BUILD_DNS:-}
  local server
  local servers=()
  rebuild_args=()
  if [[ -n "$dns" && -z "$base" ]]; then
    echo 'run-real-work: FREESIDE_REAL_RUN_BUILD_DNS requires FREESIDE_REAL_RUN_BASE_BUILD_REF' >&2
    return 2
  fi
  if [[ "$dns" == *$'\n'* || "$dns" == *$'\r'* ]]; then
    echo 'run-real-work: FREESIDE_REAL_RUN_BUILD_DNS must be on one line' >&2
    return 2
  fi
  if [[ -n "$base" ]]; then
    # Match projectimage's startup shape checks before a durable run exists.
    if [[ "$base" == -* || "$base" == *[$' \t\r\n@']* || "$base" == *://* ]]; then
      echo 'run-real-work: FREESIDE_REAL_RUN_BASE_BUILD_REF must be a local tag without whitespace, @, a URL scheme, or a leading -' >&2
      return 2
    fi
    rebuild_args=(-base-build-ref "$base")
    if [[ -n "${FREESIDE_REAL_RUN_BUILD_PROXY:-}" ]]; then
      rebuild_args+=(-build-proxy "$FREESIDE_REAL_RUN_BUILD_PROXY")
    fi
    # read splits only spaces/tabs, with no globbing or shell evaluation.
    IFS=$' \t' read -r -a servers <<< "$dns"
    for server in ${servers[@]+"${servers[@]}"}; do
      if [[ "$server" == -* ]]; then
        echo 'run-real-work: FREESIDE_REAL_RUN_BUILD_DNS entries must not begin with -' >&2
        return 2
      fi
      # The caller expands this array at the daemon launch, never at preflight.
      # shellcheck disable=SC2034
      rebuild_args+=(-build-dns "$server")
    done
  fi
  return 0
}
