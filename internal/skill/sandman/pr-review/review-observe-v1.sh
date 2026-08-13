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
repository=$(printf '%s\n' "$request_json" | jq -r '.repository')
pull_request=$(printf '%s\n' "$request_json" | jq -r '.pull_request')
head_sha=$(printf '%s\n' "$request_json" | jq -r '.head_sha')
trigger_id=$(printf '%s\n' "$request_json" | jq -r '.trigger_id')
trigger_prefix=$(printf '%s\n' "$request_json" | jq -r '.trigger_prefix')
trigger_created_at=$(printf '%s\n' "$request_json" | jq -r '.trigger_created_at')

if ! printf '%s\n' "$request_json" | jq -e '
	.protocol == "review-wait/v1" and
	type == "object" and
	(.repository | type == "string" and length > 0) and
	(.pull_request | type == "number" and floor == . and . > 0) and
	(.head_sha | type == "string" and length > 0) and
	(.trigger_id | type == "string" and length > 0) and
	(.trigger_prefix | type == "string" and length > 0) and
	(.trigger_created_at | type == "string" and length > 0) and
	(.confirmed_at | type == "string" and length > 0) and
	(.started_at | type == "string" and length > 0) and
	(.deadline_at | type == "string" and length > 0) and
	((.started_unix_seconds // (.deadline_unix_seconds - .effective_timeout_seconds)) | type == "number" and floor == . and . >= 0) and
	(.effective_timeout_seconds | type == "number" and floor == . and . > 0) and
	(.deadline_at | type == "string" and length > 0) and
	(.deadline_unix_seconds | type == "number" and floor == . and . > 0) and
	(.poll_plan | type == "array" and length > 0 and all(.[]; type == "number" and floor == . and . >= 0)) and
	(.deadline_unix_seconds == ((.started_unix_seconds // (.deadline_unix_seconds - .effective_timeout_seconds)) + .effective_timeout_seconds))
' >/dev/null 2>&1
then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"request-envelope-invalid",snapshot:null}'
	exit 0
fi

request_deadline=$(printf '%s\n' "$request_json" | jq -r '.deadline_unix_seconds')
clock=${SANDMAN_REVIEW_WAIT_CLOCK:-}

now() {
	if [ -n "$clock" ]; then
		sh "$clock"
	else
		date +%s
	fi
}

emit_timed_out() {
	observed_head_sha=${1:-}
	jq -cn --arg observed_head_sha "$observed_head_sha" \
		'{state:"timed_out",observed_head_sha:$observed_head_sha,reason:"request-deadline-exhausted",snapshot:null}'
}

check_before_operation() {
	operation_now=$(now 2>/dev/null) || return 125
	case "$operation_now" in
		''|*[!0-9]*) return 125 ;;
	esac
	[ "$operation_now" -lt "$request_deadline" ] || return 124
}

check_deadline_or_exit() {
	check_before_operation
	check_status=$?
	case "$check_status" in
		0) return 0 ;;
		124)
			emit_timed_out "${1:-}"
			exit 0
			;;
		*)
			jq -cn '{state:"unavailable",observed_head_sha:"",reason:"clock-unavailable",snapshot:null}'
			exit 0
			;;
	esac
}

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/sandman-review-observe.XXXXXX") || exit 2
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
view_file=$tmp_dir/view.json
reviews_file=$tmp_dir/reviews.json
inline_file=$tmp_dir/inline.json

check_deadline_or_exit
if ! gh pr view "$pull_request" --repo "$repository" --json headRefOid,comments,reviewDecision,mergeStateStatus >"$view_file" 2>/dev/null; then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"pull-request-view-failed",snapshot:null}'
	exit 0
fi

observed_head=$(jq -r '.headRefOid // ""' "$view_file")
check_deadline_or_exit "$observed_head"
if ! gh api "repos/$repository/pulls/$pull_request/reviews" --paginate >"$reviews_file" 2>/dev/null; then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"formal-reviews-unavailable",snapshot:null}'
	exit 0
fi

check_deadline_or_exit "$observed_head"
if ! gh api "repos/$repository/pulls/$pull_request/comments" --paginate >"$inline_file" 2>/dev/null; then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"inline-comments-unavailable",snapshot:null}'
	exit 0
fi

if ! jq -e '
	type == "object" and
	(.headRefOid | type == "string" and length > 0) and
	(.comments | type == "array") and
	all(.comments[]; type == "object")
' "$view_file" >/dev/null 2>&1
then
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"pull-request-view-invalid",snapshot:null}'
	exit 0
fi

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

reviews_json=$(jq -s 'if length > 0 and all(.[]; type == "array" and all(.[]; type == "object")) then add else error("formal reviews must be arrays of objects") end' "$reviews_file" 2>/dev/null) || {
	jq -cn '{state:"unavailable",observed_head_sha:"",reason:"formal-reviews-invalid",snapshot:null}'
	exit 0
}
inline_json=$(jq -s 'if length > 0 and all(.[]; type == "array" and all(.[]; type == "object")) then add else error("inline comments must be arrays of objects") end' "$inline_file" 2>/dev/null) || {
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
	--arg deadline_at "$(printf '%s\n' "$request_json" | jq -r '.deadline_at // ""')" \
	--argjson deadline_unix_seconds "$(printf '%s\n' "$request_json" | jq -r '.deadline_unix_seconds // 0')" \
	--argjson reviews "$reviews_json" \
	--argjson inline "$inline_json" \
	--slurpfile view "$view_file" \
'
	def event_timestamp($source):
		if $source == "top_level" then (.createdAt // .created_at // "")
		elif $source == "formal_review" then (.submitted_at // .submittedAt // .created_at // .createdAt // "")
		elif $source == "inline_comment" then (.created_at // .createdAt // "")
		else "" end;
	def timestamp_parts:
		if type != "string" or (test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$") | not) then
			null
		else
			capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $parts |
			(try (($parts.base + "Z") | fromdateiso8601) catch null) as $epoch |
			if $epoch == null or (($epoch | todateiso8601) != ($parts.base + "Z")) then
				null
			else
				{epoch: $epoch, key: ($parts.base + "." + (((($parts.fraction // "") + "000000000")[0:9])) + "Z")}
			end
		end;
	def parsed_event_timestamp($source):
		event_timestamp($source) | timestamp_parts;
	def review_state:
		if (.state? | type) == "string" then (.state | ascii_upcase) else "" end;
	def body:
		if (.body? | type) == "string" then .body else "" end;
	def event_id:
		if (.id? | type) == "string" then (if (.id | length) > 0 then .id else "" end)
		elif (.id? | type) == "number" then (.id | tostring)
		else "" end;
	def require_event_id:
		event_id as $id | if $id == "" then error("missing event id") else .id = $id end;
	def commit_id:
		if (.commit_id? | type) == "string" then .commit_id
		elif (.commitId? | type) == "string" then .commitId
		else "" end;
	def head_status($head):
		(commit_id) as $commit |
		if $commit == "" then "unknown"
		elif $commit == $head then "current"
		else "stale" end;
	def in_window($start; $window_end; $deadline; $source):
		(parsed_event_timestamp($source)) as $event |
		if $event == null then false
		else
			$event.key > $start and
			$event.key <= $deadline and
			($window_end == null or $event.key < $window_end)
		end;
	def with_metadata($source; $head):
		require_event_id |
		.id = event_id |
		.source = $source |
		.response_timestamp = (parsed_event_timestamp($source) | .key) |
		.head_status = (if $source == "top_level" then "current" else head_status($head) end);

	($trigger_created_at | timestamp_parts) as $trigger_parts |
	(($deadline_unix_seconds | todateiso8601) | sub("Z$"; ".000000000Z")) as $deadline_key |
	if $trigger_parts == null or $deadline_key < $trigger_parts.key then
		error("invalid request timing")
	else
		($trigger_parts.key) as $trigger_time |
		($view[0].comments) as $comments |
		[
			$comments[]
			| select((body | startswith($trigger_prefix)))
			| (parsed_event_timestamp("top_level")) as $event
			| if $event == null then error("invalid next trigger timestamp")
			  else
				select(((if (.url? | type) == "string" then .url else "" end) != $trigger_id or (event_timestamp("top_level") != $trigger_created_at)) and event_id != $trigger_id)
				| select($event.key >= $trigger_time)
				| if $event.key == $trigger_time then error("ambiguous trigger boundary")
				  else
					require_event_id
					| {id: event_id, url: (if (.url? | type) == "string" then .url else "" end), body: body, created_at: $event.key, _timestamp: $event.key}
				  end
			  end
		] | sort_by(._timestamp) | .[0] as $next_trigger_record |
		($next_trigger_record._timestamp // null) as $next_trigger_time |
		($next_trigger_record | if . == null then null else del(._timestamp) end) as $next_trigger |
		[
			$comments[]
			| select((.body? | type) == "string")
			| select(in_window($trigger_time; $next_trigger_time; $deadline_key; "top_level"))
			| select((body | startswith($trigger_prefix)) | not)
			| with_metadata("top_level"; $head_sha)
		] as $top_level |
		[
			$reviews[]
			| select(review_state == "COMMENTED" or review_state == "APPROVED" or review_state == "CHANGES_REQUESTED")
			| select(in_window($trigger_time; $next_trigger_time; $deadline_key; "formal_review"))
			| with_metadata("formal_review"; $head_sha)
		] as $formal_reviews |
		[
			$inline[]
			| select(in_window($trigger_time; $next_trigger_time; $deadline_key; "inline_comment"))
			| with_metadata("inline_comment"; $head_sha)
		] as $inline_comments |
		({top_level: ($top_level | length), formal_reviews: ($formal_reviews | length), inline_comments: ($inline_comments | length)}) as $response_counts |
		[
			$formal_reviews[]
			| select(review_state == "CHANGES_REQUESTED")
		] as $requested_changes |
		[
			$formal_reviews[]
			| select(review_state == "APPROVED" and .head_status == "current")
		] as $approval_evidence |
		[
			$formal_reviews[]
			| select(review_state == "APPROVED" and .head_status != "current")
		] as $ambiguous_approvals |
		(if ($requested_changes | length) > 0 then "changes_requested"
			 elif ($approval_evidence | length) > 0 then "approved"
			 elif ($ambiguous_approvals | length) > 0 then "ambiguous"
			 else "none" end) as $formal_decision |
		(if $next_trigger == null then "active" else "superseded" end) as $request_state |
		(if $request_state == "superseded" then "pending"
			 elif ($requested_changes | length) > 0 then "changes_requested"
			 elif ($approval_evidence | length) > 0 then "approved"
			 elif ($ambiguous_approvals | length) > 0 then "pending"
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
				end: (if $next_trigger == null then null else $next_trigger.created_at end),
				deadline_at: $deadline_at,
				deadline_unix_seconds: $deadline_unix_seconds,
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
has_response=$(printf '%s' "$classification_json" | jq -r 'if .request_state == "superseded" or (.response_counts.top_level + .response_counts.formal_reviews + .response_counts.inline_comments) > 0 then "responded" else "pending" end')
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
