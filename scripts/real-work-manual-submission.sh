#!/usr/bin/env bash
# Stage the optional operator input and construct the daemon-only arguments.
# This is sourced by run-real-work.sh, after its private session is created.

real_work_stage_manual_submission() {
  local explicit=$1 retained=$2 session=$3
  manual_submission_file=$(python3 "$session/real-work-retained.py" stage-manual \
    "$explicit" "$retained" "$session") || return
  manual_submission_args=()
  if [[ -n "$manual_submission_file" ]]; then
    # The caller expands this array at the daemon launch, never at preflight.
    # shellcheck disable=SC2034
    manual_submission_args=(-manual-submission-config "$manual_submission_file")
  fi
}
