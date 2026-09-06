#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/run-real-work-supervision.sh
source "$SCRIPT_DIR/run-real-work-supervision.sh"

fixture_root=$(mktemp -d)
trap 'rm -rf "$fixture_root"' EXIT
stub_root=$fixture_root/stub
mkdir -p "$stub_root"
export STUB_ROOT=$stub_root

stub=$fixture_root/freesided
cat >"$stub" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
run_id=""
while (($#)); do
	case "$1" in
	-run)
		run_id=$2
		shift 2
		;;
	*) shift ;;
	esac
done
sequence=$STUB_ROOT/$run_id
count_file=$STUB_ROOT/$run_id.count
count=0
[[ ! -f "$count_file" ]] || read -r count <"$count_file"
count=$((count + 1))
printf '%s\n' "$count" >"$count_file"
line=$(sed -n "${count}p" "$sequence")
[[ -n "$line" ]] || line=$(tail -1 "$sequence")
if [[ "$line" == "__UNREADABLE__" ]]; then
	echo 'freesided follow: observe snapshot: begin: database is locked (5) (SQLITE_BUSY)' >&2
	exit 1
fi
printf '%s\n' "$line"
STUB
chmod +x "$stub"

identity='"lineage":{"campaign_id":"campaign-1","attempt_number":2,"kind":"retry","parent_run_id":"run-parent","source_digest":"sha256:source","approved_spec_digest":"sha256:spec","specification_run_id":"run-spec","implementation_run_id":"run-impl"},"admission":{"invocation_id":"inv-impl","stage":"implementation","image_ref":"registry/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","image_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base":{"repo":"owner/repo","repository_id":42,"base_ref":"main","base_sha":"base-sha"},"trust_profile_digest":"sha256:profile","review_configuration_digest":"sha256:review-config"},"last_stage":"implementation"'

snapshot() {
	local state=$1 outcome=$2 extra=${3:-}
	printf '{"run_id":"run-impl","state":"%s","outcome":"%s",%s,"attention_items":[]%s}\n' \
		"$state" "$outcome" "$identity" "$extra"
}

write_sequence() {
	local run_id=$1
	shift
	printf '%s\n' "$@" >"$stub_root/$run_id"
	rm -f "$stub_root/$run_id.count"
}

assert_contains() {
	local path=$1 want=$2
	if ! grep -Fq -- "$want" "$path"; then
		echo "missing '$want' in $path" >&2
		cat "$path" >&2
		exit 1
	fi
}

run_case() {
	local name=$1 specification=$2 timeout=$3 expected=$4
	local case_dir=$fixture_root/$name status
	mkdir -p "$case_dir"
	set +e
	real_work_supervise "$stub" /state/freeside.db "$specification" run-impl "" \
		"$timeout" "$case_dir/snapshot.json" 0.01 2>"$case_dir/stderr"
	status=$?
	set -e
	if [[ "$status" -ne "$expected" ]]; then
		echo "$name: status=$status, want $expected" >&2
		cat "$case_dir/stderr" >&2
		exit 1
	fi
	assert_contains "$case_dir/snapshot.json" '"source_digest":"sha256:source"'
	assert_contains "$case_dir/snapshot.json" '"approved_spec_digest":"sha256:spec"'
	assert_contains "$case_dir/snapshot.json" '"implementation_run_id":"run-impl"'
	assert_contains "$case_dir/snapshot.json" '"trust_profile_digest":"sha256:profile"'
	assert_contains "$case_dir/snapshot.json" '"base_sha":"base-sha"'
	assert_contains "$case_dir/snapshot.json" '"image_digest":"sha256:aaaaaaaa'
	assert_contains "$case_dir/snapshot.json" '"review_configuration_digest":"sha256:review-config"'
	assert_contains "$case_dir/snapshot.json" '"last_stage":"implementation"'
}

# Success follows both identities and reaches one exact published run.
write_sequence run-spec \
	"$(snapshot waiting_for_specification_approval pending)" \
	"$(snapshot implementation_bound pending)"
write_sequence run-impl \
	"$(snapshot pending pending)" \
	"$(snapshot publication_ready published)" \
	"$(snapshot published published)"
run_case success run-spec 3 0
assert_contains "$fixture_root/success/stderr" 'specification run=run-spec state=waiting_for_specification_approval'
assert_contains "$fixture_root/success/stderr" 'implementation run=run-impl state=published'
assert_contains "$fixture_root/success/stderr" 'implementation run=run-impl state=publication_ready'

# Rejection and specification execution failure are terminal immediately and
# retain their distinct terminal classes plus the relevant AttentionItem.
rejected_extra=',"terminal":"canceled","last_attention_item":{"id":"spec-approval-1","type":"spec_approval","status":"superseded","requested_decision":[]}'
write_sequence run-spec "$(snapshot failed failed "$rejected_extra")"
run_case rejection run-spec 3 1
assert_contains "$fixture_root/rejection/stderr" 'terminal=canceled'

failure_extra=',"terminal":"failed","last_attention_item":{"id":"execution-failure-1","type":"execution_failure","status":"open","requested_decision":["retry"]}'
write_sequence run-spec "$(snapshot failed failed "$failure_extra")"
run_case specification-failure run-spec 3 1
assert_contains "$fixture_root/specification-failure/stderr" 'attention=execution-failure-1 type=execution_failure'

# Approval waiting is a first-class state rather than a silent retry.
write_sequence run-spec \
	"$(snapshot waiting_for_specification_approval pending)" \
	"$(snapshot implementation_bound pending)"
write_sequence run-impl "$(snapshot published published)"
run_case approval-wait run-spec 3 0
assert_contains "$fixture_root/approval-wait/stderr" 'state=waiting_for_specification_approval'

# An implementation terminal failure reports the implementation identity.
write_sequence run-spec "$(snapshot implementation_bound pending)"
write_sequence run-impl "$(snapshot failed failed ',"terminal":"failed"')"
run_case implementation-failure run-spec 3 1
assert_contains "$fixture_root/implementation-failure/stderr" 'implementation run=run-impl ended outcome=failed terminal=failed'

# Parked review keeps reconciliation live, prints the exact observation
# command once, and then reaches publication after the hold is resolved.
parked_extra=',"last_attention_item":{"id":"review-config-1","type":"review_configuration","status":"open","requested_decision":["adopt_review_configuration"]}'
write_sequence run-spec "$(snapshot implementation_bound pending)"
write_sequence run-impl \
	"$(snapshot attention_required pending "$parked_extra")" \
	"$(snapshot attention_required pending "$parked_extra")" \
	"$(snapshot published published)"
run_case parked-review run-spec 3 0
assert_contains "$fixture_root/parked-review/stderr" 'attention required for implementation run=run-impl; daemon reconciliation remains active'
assert_contains "$fixture_root/parked-review/stderr" 'freesided resume -db /state/freeside.db -run run-impl'
if [[ "$(grep -Fc 'freesided resume -db /state/freeside.db -run run-impl' "$fixture_root/parked-review/stderr")" -ne 1 ]]; then
	echo "parked review printed its observation command more than once" >&2
	exit 1
fi
assert_contains "$fixture_root/parked-review/snapshot.json" '"state":"published"'

# Resume starts from the supplied implementation identity and never observes
# or resubmits the specification lane.
rm -f "$stub_root/run-spec.count"
write_sequence run-impl "$(snapshot published published)"
run_case resume "" 3 0
if [[ -e "$stub_root/run-spec.count" ]]; then
	echo "resume observed the specification lane" >&2
	exit 1
fi

# Timeout is bounded and leaves the last exact snapshot as its diagnostic.
write_sequence run-impl "$(snapshot pending pending)"
run_case timeout "" 1 124
assert_contains "$fixture_root/timeout/stderr" 'timed out supervising implementation run=run-impl'
assert_contains "$fixture_root/timeout/snapshot.json" '"state":"pending"'

# A lock collision against the live database is a transient, not a run
# failure: the supervisor retries and the run still reaches publication.
write_sequence run-impl \
	"__UNREADABLE__" \
	"__UNREADABLE__" \
	"$(snapshot publication_ready published)" \
	"$(snapshot published published)"
run_case transient-observation "" 3 0
assert_contains "$fixture_root/transient-observation/stderr" 'transient observation failure 1/10'
assert_contains "$fixture_root/transient-observation/stderr" 'transient observation failure 2/10'
assert_contains "$fixture_root/transient-observation/stderr" 'implementation run=run-impl state=published'

# Observation that never gets through is still terminal, bounded by the
# consecutive-failure budget rather than retried until the global timeout.
write_sequence run-impl "__UNREADABLE__"
unreadable_dir=$fixture_root/unreadable
mkdir -p "$unreadable_dir"
set +e
FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES=3 \
	real_work_supervise "$stub" /state/freeside.db "" run-impl "" \
	30 "$unreadable_dir/snapshot.json" 0.01 2>"$unreadable_dir/stderr"
unreadable_status=$?
set -e
if [[ "$unreadable_status" -ne 1 ]]; then
	echo "unreadable: status=$unreadable_status, want 1" >&2
	cat "$unreadable_dir/stderr" >&2
	exit 1
fi
assert_contains "$unreadable_dir/stderr" 'could not observe implementation run=run-impl after 3 consecutive attempts'

# A malformed or zero-padded numeric override is rejected before supervision
# rather than aborting under nounset or erroring on octal every observation.
assert_invalid_config() {
	local name=$1 max_override=$2 timeout=$3 want=$4
	local case_dir=$fixture_root/$name status
	mkdir -p "$case_dir"
	write_sequence run-impl "$(snapshot published published)"
	set +e
	if [[ -n "$max_override" ]]; then
		FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES=$max_override \
			real_work_supervise "$stub" /state/freeside.db "" run-impl "" \
			"$timeout" "$case_dir/snapshot.json" 0.01 2>"$case_dir/stderr"
	else
		real_work_supervise "$stub" /state/freeside.db "" run-impl "" \
			"$timeout" "$case_dir/snapshot.json" 0.01 2>"$case_dir/stderr"
	fi
	status=$?
	set -e
	if [[ "$status" -ne 2 ]]; then
		echo "$name: status=$status, want 2" >&2
		cat "$case_dir/stderr" >&2
		exit 1
	fi
	assert_contains "$case_dir/stderr" "$want"
}

assert_invalid_config max-observation-nonnumeric abc 3 \
	'FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES must be a positive integer, got: abc'
assert_invalid_config max-observation-octal 09 3 \
	'FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES must be a positive integer, got: 09'
assert_invalid_config timeout-nonnumeric "" abc \
	'supervision timeout must be a positive integer, got: abc'
assert_invalid_config timeout-octal "" 09 \
	'supervision timeout must be a positive integer, got: 09'

echo "run-real-work supervision fixtures passed"
