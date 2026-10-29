#!/bin/sh

# ci-rerun-v1 is an agent-invoked, fail-closed recovery action for one
# diagnosed transient failure. It never retries an ambiguous mutation.

set -u

repository=
pull_request=
head_sha=
tested_sha=
run_id=
job_id=
diagnosis=

usage() {
	cat >&2 <<'EOF'
usage: ci-rerun-v1.sh --repository OWNER/REPO --pull-request N --head-sha SHA --tested-sha SHA --run-id ID --job-id DATABASE_ID --diagnosis transient-provider-timeout|transient-network-timeout
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--repository) [ "$#" -ge 2 ] || { usage; exit 2; }; repository=$2; shift 2 ;;
		--pull-request) [ "$#" -ge 2 ] || { usage; exit 2; }; pull_request=$2; shift 2 ;;
		--head-sha) [ "$#" -ge 2 ] || { usage; exit 2; }; head_sha=$2; shift 2 ;;
		--tested-sha) [ "$#" -ge 2 ] || { usage; exit 2; }; tested_sha=$2; shift 2 ;;
		--run-id) [ "$#" -ge 2 ] || { usage; exit 2; }; run_id=$2; shift 2 ;;
		--job-id) [ "$#" -ge 2 ] || { usage; exit 2; }; job_id=$2; shift 2 ;;
		--diagnosis) [ "$#" -ge 2 ] || { usage; exit 2; }; diagnosis=$2; shift 2 ;;
		-h|--help) usage; exit 0 ;;
		*) usage; exit 2 ;;
	esac
done

refuse() {
	emit refused "$1"
	exit 1
}

emit() {
	state=$1
	reason=$2
	observed_attempt=${3:-0}
	jq -cn \
		--arg repository "$repository" \
		--arg head_sha "$head_sha" \
		--arg tested_sha "$tested_sha" \
		--arg diagnosis "$diagnosis" \
		--arg reason "$reason" \
		--arg state "$state" \
		--arg run_id "$run_id" \
		--arg job_id "$job_id" \
		--argjson pull_request "${pull_request:-0}" \
		--argjson run_attempt "${observed_attempt:-0}" \
		'{protocol:"ci-rerun/v1",state:$state,reason:$reason,repository:$repository,
		  pull_request:$pull_request,source_head_sha:$head_sha,tested_sha:$tested_sha,
		  diagnosis:$diagnosis,run_id:$run_id,job_id:$job_id,observed_attempt:$run_attempt}'
}

waiting() {
	emit waiting "$1" "${2:-0}"
	exit 0
}

uncertain() {
	emit uncertain "$1" "${2:-0}"
	exit 1
}

case "$repository" in
	*/*) owner=${repository%%/*}; repo=${repository#*/}; case "$repo" in */*|'') usage; exit 2;; esac ;;
	*) usage; exit 2 ;;
esac
case "$owner$repo" in *[!A-Za-z0-9_.-]*|'') usage; exit 2;; esac
for numeric_id in "$pull_request" "$run_id" "$job_id"; do
	case "$numeric_id" in ''|*[!0-9]*) usage; exit 2;; esac
	case "$numeric_id" in *[1-9]*) ;; *) usage; exit 2;; esac
done
[ -n "$head_sha" ] && [ -n "$tested_sha" ] || { usage; exit 2; }
case "$diagnosis" in
	transient-provider-timeout|transient-network-timeout) ;;
	*) refuse "transient-diagnosis-required" ;;
esac

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/sandman-ci-rerun.XXXXXX") || refuse "temporary-directory-failed"
trap 'rm -rf "$tmp_dir"' EXIT
trap 'exit 130' HUP INT TERM
pr_file=$tmp_dir/pr.json
run_file=$tmp_dir/run.json
jobs_file=$tmp_dir/jobs.json
after_run_file=$tmp_dir/after-run.json
after_jobs_file=$tmp_dir/after-jobs.json
after_pr_file=$tmp_dir/after-pr.json

api_pr="repos/$repository/pulls/$pull_request"
api_run="repos/$repository/actions/runs/$run_id"
api_jobs="$api_run/jobs?filter=latest&per_page=100"

if ! gh api "$api_pr" >"$pr_file" 2>/dev/null; then
	refuse "pull-request-lookup-failed"
fi
if ! jq -e --arg repository "$repository" --arg head "$head_sha" --argjson number "$pull_request" '
	type == "object" and .number == $number and .state == "open" and
	.head.sha == $head and (.merged_at == null) and
	((.base.repo.full_name // "" | ascii_downcase) == ($repository | ascii_downcase))
' "$pr_file" >/dev/null 2>&1; then
	refuse "pull-request-or-source-head-mismatch"
fi

if ! gh api "$api_run" >"$run_file" 2>/dev/null; then
	refuse "workflow-run-lookup-failed"
fi
if ! jq -e --arg repository "$repository" --arg head "$head_sha" --arg tested "$tested_sha" \
	--argjson pull_request "$pull_request" --argjson run_id "$run_id" '
	type == "object" and .id == $run_id and
	((.repository.full_name // "" | ascii_downcase) == ($repository | ascii_downcase)) and
	.event == "pull_request" and .head_sha == $tested and
	([.pull_requests[]? | select(.number == $pull_request and .head.sha == $head)] | length) == 1 and
	(.run_attempt | type == "number" and . >= 1) and
	(.status | type == "string" and length > 0)
' "$run_file" >/dev/null 2>&1; then
	refuse "workflow-run-linkage-mismatch"
fi
run_attempt=$(jq -er '.run_attempt' "$run_file") || refuse "workflow-attempt-invalid"
run_status=$(jq -er '.status' "$run_file") || refuse "workflow-status-invalid"
run_conclusion=$(jq -r '.conclusion // ""' "$run_file")

if ! gh api --paginate "$api_jobs" >"$tmp_dir/jobs-pages.jsonl" 2>/dev/null; then
	refuse "workflow-jobs-lookup-failed"
fi
if ! jq -s '[.[] | .jobs[]?]' "$tmp_dir/jobs-pages.jsonl" >"$jobs_file"; then
	refuse "workflow-jobs-invalid"
fi
job_file=$tmp_dir/job.json
if ! jq -e --argjson job_id "$job_id" --argjson run_id "$run_id" --argjson attempt "$run_attempt" '
	[.[] | select(.id == $job_id and .run_id == $run_id and .run_attempt == $attempt)] |
	length == 1 and
	(.[0] | (.status == "completed" and (.conclusion == "failure" or .conclusion == "timed_out")) or
	       (.status == "queued" or .status == "in_progress"))
' "$jobs_file" >/dev/null 2>&1; then
	refuse "failed-job-not-in-current-attempt"
fi
jq -c --argjson job_id "$job_id" --argjson run_id "$run_id" --argjson attempt "$run_attempt" \
	'[.[] | select(.id == $job_id and .run_id == $run_id and .run_attempt == $attempt)][0]' \
	"$jobs_file" >"$job_file" || refuse "failed-job-evidence-invalid"
job_status=$(jq -er '.status' "$job_file") || refuse "failed-job-status-invalid"
job_conclusion=$(jq -r '.conclusion // ""' "$job_file")
job_name=$(jq -er '.name | strings | select(length > 0)' "$job_file") || refuse "failed-job-name-invalid"
job_workflow_name=$(jq -r '.workflow_name // ""' "$job_file")
prior_quality_gate_id=$(jq -r --argjson run_id "$run_id" --argjson attempt "$run_attempt" \
	'[.[] | select(.run_id == $run_id and .run_attempt == $attempt and .name == "quality-gate") | .id][0] // 0' \
	"$jobs_file") || refuse "quality-gate-evidence-invalid"

case "$job_status" in
	queued|in_progress) waiting "recovery-already-active" "$run_attempt" ;;
esac
case "$run_status" in
	queued|in_progress) waiting "workflow-still-running" "$run_attempt" ;;
esac
case "$run_status:$job_status:$run_conclusion:$job_conclusion" in
	completed:completed:failure:failure|completed:completed:failure:timed_out) ;;
	completed:completed:*:failure|completed:completed:*:timed_out) ;;
	*) refuse "current-execution-is-not-rerunnable" ;;
esac

# An individual-job rerun re-executes that job and its dependent jobs, including
# the aggregate gate. Pass the API database ID obtained from the jobs endpoint.
post_error=$tmp_dir/post-error.txt
post_succeeded=1
if ! gh api --method POST "repos/$repository/actions/jobs/$job_id/rerun" >"$tmp_dir/post-response.json" 2>"$post_error"; then
	post_succeeded=0
fi

# Always read back after one mutation attempt. A lost/ambiguous response must
# never cause a second POST.
if ! gh api "$api_run" >"$after_run_file" 2>/dev/null; then
	uncertain "rerun-readback-failed" "$run_attempt"
fi
if ! gh api "$api_pr" >"$after_pr_file" 2>/dev/null; then
	uncertain "pull-request-readback-failed" "$run_attempt"
fi
if ! gh api --paginate "$api_jobs" >"$tmp_dir/after-jobs-pages.jsonl" 2>/dev/null; then
	uncertain "rerun-jobs-readback-failed" "$run_attempt"
fi
if ! jq -s '[.[] | .jobs[]?]' "$tmp_dir/after-jobs-pages.jsonl" >"$after_jobs_file"; then
	uncertain "rerun-jobs-readback-invalid" "$run_attempt"
fi

if ! jq -e --arg repository "$repository" --arg head "$head_sha" --arg tested "$tested_sha" \
	--argjson pull_request "$pull_request" --argjson run_id "$run_id" '
	type == "object" and .id == $run_id and
	((.repository.full_name // "" | ascii_downcase) == ($repository | ascii_downcase)) and
	.event == "pull_request" and
	.head_sha == $tested and
	([.pull_requests[]? | select(.number == $pull_request and .head.sha == $head)] | length) == 1
' "$after_run_file" >/dev/null 2>&1; then
	uncertain "rerun-readback-identity-changed" "$run_attempt"
fi
if ! jq -e --arg repository "$repository" --arg head "$head_sha" --argjson pull_request "$pull_request" '
	type == "object" and .number == $pull_request and .state == "open" and
	.head.sha == $head and (.merged_at == null) and
	((.base.repo.full_name // "" | ascii_downcase) == ($repository | ascii_downcase))
' "$after_pr_file" >/dev/null 2>&1; then
	uncertain "pull-request-readback-identity-changed" "$run_attempt"
fi
after_attempt=$(jq -er '.run_attempt' "$after_run_file") || uncertain "rerun-attempt-readback-invalid" "$run_attempt"
after_status=$(jq -er '.status' "$after_run_file") || uncertain "rerun-status-readback-invalid" "$run_attempt"

if [ "$after_attempt" -gt "$run_attempt" ] &&
	jq -e --argjson job_id "$job_id" --argjson run_id "$run_id" --argjson attempt "$after_attempt" \
		--arg job_name "$job_name" --arg workflow_name "$job_workflow_name" '
		[.[] | select(.id != $job_id and .run_id == $run_id and .run_attempt == $attempt and
			.name == $job_name and ($workflow_name == "" or .workflow_name == $workflow_name) and
			(.status == "queued" or .status == "in_progress" or .status == "completed"))] | length == 1
	' "$after_jobs_file" >/dev/null 2>&1 &&
	jq -e --argjson run_id "$run_id" --argjson attempt "$after_attempt" --argjson previous_id "$prior_quality_gate_id" '
		[.[] | select(.id != $previous_id and .run_id == $run_id and .run_attempt == $attempt and
			.name == "quality-gate" and
			(.status == "queued" or .status == "in_progress" or .status == "completed"))] | length == 1
	' "$after_jobs_file" >/dev/null 2>&1; then
	emit confirmed "selected-job-and-quality-gate-rerun-observed" "$after_attempt"
	exit 0
fi

if [ "$post_succeeded" -eq 0 ] && grep -Eiq 'workflow run containing this job is already running' "$post_error" &&
	{ [ "$after_status" = "in_progress" ] || [ "$after_status" = "queued" ]; } &&
	jq -e --argjson job_id "$job_id" --argjson run_id "$run_id" --argjson attempt "$run_attempt" '
		[.[] | select(.id == $job_id and .run_id == $run_id and .run_attempt == $attempt and
			.status == "completed" and (.conclusion == "failure" or .conclusion == "timed_out"))] | length == 1
	' "$after_jobs_file" >/dev/null 2>&1 &&
	jq -e --argjson job_id "$job_id" --argjson run_id "$run_id" --argjson attempt "$run_attempt" '
		[.[] | select(.id != $job_id and .run_id == $run_id and .run_attempt == $attempt and
			(.status == "queued" or .status == "in_progress"))] | length > 0
	' "$after_jobs_file" >/dev/null 2>&1; then
	waiting "workflow-still-running" "$after_attempt"
fi
if [ "$post_succeeded" -eq 0 ]; then
	if grep -Eiq '403|resource not accessible|permission|not authorized' "$post_error"; then
		refuse "rerun-authorization-denied"
	fi
	uncertain "rerun-submission-not-confirmed" "$after_attempt"
fi
uncertain "rerun-submission-not-confirmed" "$after_attempt"
