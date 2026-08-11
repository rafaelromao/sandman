#!/bin/sh

# review-observe-v1 gathers the response surfaces without deciding whether a
# response is approval, requested changes, or actionable feedback.

set -u

request_file=
while [ "$#" -gt 0 ]; do
	case "$1" in
		--request-file)
			[ "$#" -ge 2 ] || exit 2
			request_file=$2
			shift 2
			;;
		*)
			exit 2
			;;
	esac
done

if [ -z "$request_file" ] || [ ! -f "$request_file" ]; then
	exit 2
fi

request_json=$(jq -c . "$request_file" 2>/dev/null) || exit 2
repository=$(jq -r '.repository' "$request_file")
pull_request=$(jq -r '.pull_request' "$request_file")
head_sha=$(jq -r '.head_sha' "$request_file")
trigger_id=$(jq -r '.trigger_id' "$request_file")
trigger_prefix=$(jq -r '.trigger_prefix' "$request_file")
trigger_created_at=$(jq -r '.trigger_created_at' "$request_file")

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/sandman-review-observe.XXXXXX") || exit 2
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
view_file=$tmp_dir/view.json
reviews_file=$tmp_dir/reviews.json
inline_file=$tmp_dir/inline.json

if ! gh pr view "$pull_request" --repo "$repository" --json headRefOid,comments,reviewDecision,mergeStateStatus >"$view_file" 2>/dev/null; then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"pull-request-view-failed",snapshot:null}'
	exit 0
fi
if ! gh api "repos/$repository/pulls/$pull_request/reviews" --paginate >"$reviews_file" 2>/dev/null; then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"formal-reviews-unavailable",snapshot:null}'
	exit 0
fi
if ! gh api "repos/$repository/pulls/$pull_request/comments" --paginate >"$inline_file" 2>/dev/null; then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"inline-comments-unavailable",snapshot:null}'
	exit 0
fi

observed_head=$(jq -r '.headRefOid // ""' "$view_file")
if [ "$observed_head" != "$head_sha" ]; then
	jq -cn --arg observed_head_sha "$observed_head" \
		'{state:"unavailable",observed_head_sha:$observed_head_sha,reason:"head-mismatch",snapshot:null}'
	exit 0
fi

if ! jq -e --arg trigger_id "$trigger_id" --arg prefix "$trigger_prefix" --arg created_at "$trigger_created_at" '
	any(.comments[]?;
		(((.url // "") == $trigger_id) or ((.id | tostring) == $trigger_id)) and
		((.body // "") | startswith($prefix)) and
		((.createdAt // .created_at // "") == $created_at))
' "$view_file" >/dev/null 2>&1
then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"confirmed-trigger-not-found",snapshot:null}'
	exit 0
fi

reviews_json=$(jq -s 'if length == 1 and (.[0] | type) == "array" then .[0] else add end' "$reviews_file" 2>/dev/null) || {
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"formal-reviews-invalid",snapshot:null}'
	exit 0
}
inline_json=$(jq -s 'if length == 1 and (.[0] | type) == "array" then .[0] else add end' "$inline_file" 2>/dev/null) || {
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"inline-comments-invalid",snapshot:null}'
	exit 0
}

classification_json=$(jq -cn \
	--arg repository "$repository" \
	--argjson pull_request "$pull_request" \
	--arg head_sha "$head_sha" \
	--arg observed_head_sha "$observed_head" \
	--arg trigger_id "$trigger_id" \
	--arg trigger_prefix "$trigger_prefix" \
	--arg trigger_created_at "$trigger_created_at" \
	--arg deadline_at "$(jq -r '.deadline_at // ""' "$request_file")" \
	--argjson deadline_unix_seconds "$(jq -r '.deadline_unix_seconds // 0' "$request_file")" \
	--argjson reviews "$reviews_json" \
	--argjson inline "$inline_json" \
	--slurpfile view "$view_file" \
'
	def valid_timestamp:
		if type == "string" then
			test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?Z$")
		else false end;
	def timestamp:
		(.createdAt // .created_at // .submitted_at // .submittedAt // "");
	def in_window($start; $window_end):
		(timestamp) as $timestamp |
		($timestamp | valid_timestamp) and
		$timestamp > $start and
		($window_end == "" or $timestamp < $window_end);
	def commit_id:
		(.commit_id // .commitId // "");
	def head_status($head):
		(commit_id) as $commit |
		if $commit == "" then "unknown"
		elif $commit == $head then "current"
		else "stale" end;
	def with_metadata($source; $head):
		.source = $source |
		.response_timestamp = timestamp |
		.head_status = (if $source == "top_level" then "current" else head_status($head) end);

	if ($trigger_created_at | valid_timestamp) | not then
		error("invalid trigger timestamp")
	else
		($view[0].comments // []) as $comments |
		[
			$comments[]?
			| (timestamp) as $timestamp
			| select(($timestamp | valid_timestamp) and $timestamp > $trigger_created_at)
			| select((.body // "") | startswith($trigger_prefix))
			| {id: ((.id // "") | tostring), url: (.url // ""), body: (.body // ""), created_at: $timestamp}
		] | sort_by(.created_at) | .[0] as $next_trigger |
		($next_trigger.created_at // "") as $next_trigger_created_at |
		[
			$comments[]?
			| select(in_window($trigger_created_at; $next_trigger_created_at))
			| select(((.body // "") | startswith($trigger_prefix)) | not)
			| with_metadata("top_level"; $head_sha)
		] as $top_level |
		[
			$reviews[]?
			| select((.state // "" | ascii_upcase) == "COMMENTED" or (.state // "" | ascii_upcase) == "APPROVED" or (.state // "" | ascii_upcase) == "CHANGES_REQUESTED")
			| select(in_window($trigger_created_at; $next_trigger_created_at))
			| with_metadata("formal_review"; $head_sha)
		] as $formal_reviews |
		[
			$inline[]?
			| select(in_window($trigger_created_at; $next_trigger_created_at))
			| with_metadata("inline_comment"; $head_sha)
		] as $inline_comments |
		({top_level: ($top_level | length), formal_reviews: ($formal_reviews | length), inline_comments: ($inline_comments | length)}) as $response_counts |
		[
			$formal_reviews[]
			| select((.state // "" | ascii_upcase) == "CHANGES_REQUESTED")
		] as $requested_changes |
		[
			$formal_reviews[]
			| select((.state // "" | ascii_upcase) == "APPROVED" and .head_status == "current")
		] as $approval_evidence |
		[
			$formal_reviews[]
			| select((.state // "" | ascii_upcase) == "APPROVED" and .head_status != "current")
		] as $ambiguous_approvals |
		(if ($requested_changes | length) > 0 then "changes_requested"
		 elif ($approval_evidence | length) > 0 then "approved"
		 elif ($ambiguous_approvals | length) > 0 then "ambiguous"
		 else "none" end) as $formal_decision |
		(if $next_trigger == null then "active" else "superseded" end) as $request_state |
		(if $request_state == "superseded" then "pending"
		 elif ($requested_changes | length) > 0 then "changes_requested"
		 elif ($approval_evidence | length) > 0 then "approved"
		 elif (($response_counts.top_level + $response_counts.formal_reviews + $response_counts.inline_comments) > 0) then "responded"
		 else "pending" end) as $decision |
		{
			protocol: "review-classification/v1",
			request: {
				repository: $repository,
				pull_request: $pull_request,
				head_sha: $head_sha,
				trigger_id: $trigger_id,
				trigger_prefix: $trigger_prefix,
				trigger_created_at: $trigger_created_at,
				deadline_at: $deadline_at,
				deadline_unix_seconds: $deadline_unix_seconds
			},
			observed_head_sha: $observed_head_sha,
			request_state: $request_state,
			decision: $decision,
			window: {
				start: $trigger_created_at,
				end: (if $next_trigger == null then null else $next_trigger_created_at end),
				next_trigger: $next_trigger
			},
			response_counts: $response_counts,
			sources: {
				top_level: $top_level,
				formal_reviews: $formal_reviews,
				inline_comments: $inline_comments
			},
			formal: {
				decision: $formal_decision,
				approval_evidence: $approval_evidence,
				ambiguous_approval_evidence: $ambiguous_approvals,
				requested_changes: $requested_changes
			},
			boundary_evidence: {
				request: {head_sha: $head_sha, deadline_at: $deadline_at, deadline_unix_seconds: $deadline_unix_seconds},
				sources: {top_level: $top_level, formal_reviews: $formal_reviews, inline_comments: $inline_comments}
			}
		}
	end
') || {
	jq -cn --arg observed_head_sha "$observed_head" \
		'{state:"unavailable",observed_head_sha:$observed_head_sha,reason:"classification-failed",snapshot:null}'
	exit 0
}

response_counts=$(printf '%s' "$classification_json" | jq -c '.response_counts')
has_response=$(printf '%s' "$response_counts" | jq -r 'if (.top_level + .formal_reviews + .inline_comments) > 0 then "responded" else "pending" end')
jq -cn \
	--arg state "$has_response" \
	--arg observed_head_sha "$observed_head" \
	--arg reason "$has_response" \
	--slurpfile view "$view_file" \
	--argjson reviews "$reviews_json" \
	--argjson inline "$inline_json" \
	--argjson counts "$response_counts" \
	--argjson classification "$classification_json" \
	'{state:$state,observed_head_sha:$observed_head_sha,reason:$reason,snapshot:{pull_request_view:$view[0],formal_reviews:$reviews,inline_comments:$inline,response_counts:$counts,classification:$classification}}'
