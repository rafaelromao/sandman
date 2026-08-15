#!/bin/sh

# review-trigger-guard-v1 is a read-only delivery preflight. It decides whether
# a command-prefixed trigger may be posted, but never writes review lifecycle
# state.

set -u

repository=
pull_request=
head_sha=
trigger_prefix=
request_file=

usage() {
	cat >&2 <<'EOF'
usage: review-trigger-guard-v1.sh --repository OWNER/REPO --pull-request N --head-sha SHA --trigger-prefix PREFIX [--request-file PATH]
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--repository)
			[ "$#" -ge 2 ] || { usage; exit 2; }
			repository=$2
			shift 2
			;;
		--pull-request)
			[ "$#" -ge 2 ] || { usage; exit 2; }
			pull_request=$2
			shift 2
			;;
		--head-sha)
			[ "$#" -ge 2 ] || { usage; exit 2; }
			head_sha=$2
			shift 2
			;;
		--trigger-prefix)
			[ "$#" -ge 2 ] || { usage; exit 2; }
			trigger_prefix=$2
			shift 2
			;;
		--request-file)
			[ "$#" -ge 2 ] || { usage; exit 2; }
			request_file=$2
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			usage
			exit 2
			;;
	esac
done

case "$pull_request" in
	''|*[!0-9]*) usage; exit 2 ;;
esac
if [ -z "$repository" ] || [ -z "$head_sha" ] || [ -z "$trigger_prefix" ]; then
	usage
	exit 2
fi

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/sandman-review-trigger.XXXXXX") || exit 2
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
view_file=$tmp_dir/view.json
comments_file=$tmp_dir/comments.json
reviews_file=$tmp_dir/reviews.json
inline_file=$tmp_dir/inline.json

emit() {
	decision=$1
	reason=$2
	observed_head=$3
	latest_trigger=${4:-null}
	jq -cn \
		--arg decision "$decision" \
		--arg reason "$reason" \
		--arg observed_head_sha "$observed_head" \
		--argjson latest_trigger "$latest_trigger" \
		'{protocol:"review-trigger/v1",decision:$decision,reason:$reason,observed_head_sha:$observed_head_sha,latest_trigger:$latest_trigger}'
}

uncertain() {
	emit uncertain "$1" "${2:-}" null
	exit 0
}

prior_trigger_id=
prior_head_sha=
prior_trigger_created_at=
if [ -n "$request_file" ]; then
	if [ ! -f "$request_file" ]; then
		uncertain request-envelope-missing
	fi
	request_json=$(jq -c . "$request_file" 2>/dev/null) || uncertain request-envelope-invalid
	if ! printf '%s\n' "$request_json" | jq -e --arg repository "$repository" --arg prefix "$trigger_prefix" --argjson pull_request "$pull_request" '
		type == "object" and
		.protocol == "review-wait/v1" and
		.repository == $repository and
		.pull_request == $pull_request and
		(.head_sha | type == "string" and length > 0) and
		(.trigger_id | type == "string" and length > 0) and
		.trigger_prefix == $prefix and
		(.trigger_created_at | type == "string" and length > 0 and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$"))
	' >/dev/null 2>&1; then
		uncertain request-envelope-invalid
	fi
	prior_trigger_id=$(printf '%s\n' "$request_json" | jq -r '.trigger_id')
	prior_head_sha=$(printf '%s\n' "$request_json" | jq -r '.head_sha')
	prior_trigger_created_at=$(printf '%s\n' "$request_json" | jq -r '.trigger_created_at')
fi

if ! gh pr view "$pull_request" --repo "$repository" --json headRefOid >"$view_file" 2>/dev/null; then
	uncertain pull-request-view-failed
fi

if ! jq -e '
	type == "object" and
	(.headRefOid | type == "string" and length > 0)
' "$view_file" >/dev/null 2>&1; then
	uncertain pull-request-view-invalid
fi

observed_head=$(jq -r '.headRefOid' "$view_file") || uncertain pull-request-head-invalid
if [ "$observed_head" != "$head_sha" ]; then
	uncertain head-changed "$observed_head"
fi

if ! gh api "repos/$repository/issues/$pull_request/comments" --paginate >"$comments_file" 2>/dev/null; then
	uncertain top-level-comments-unavailable "$observed_head"
fi
comments_json=$(jq -sc 'if length > 0 and all(.[]; type == "array" and all(.[]; type == "object")) then add else error("top-level comments must be arrays of objects") end' "$comments_file" 2>/dev/null) || uncertain top-level-comments-invalid "$observed_head"

if ! printf '%s\n' "$comments_json" | jq -c --arg prefix "$trigger_prefix" '
	def timestamp_key:
		if type != "string" or (test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$") | not) then
			null
		else
			capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2})(?<time>T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $parts |
			(try (($parts.base + $parts.time + "Z") | fromdateiso8601) catch null) as $epoch |
			if $epoch == null then null else ($parts.base + $parts.time + "." + (((($parts.fraction // "") + "000000000")[0:9])) + "Z") end
		end;
	def comment_id:
		if (.id? | type) == "string" and (.id | test("^[1-9][0-9]*$")) then .id
		elif (.id? | type) == "number" and (.id | floor == . and . > 0) then (.id | tostring)
		else ""
		end;
	def comment_url:
		if (.html_url? | type) == "string" then .html_url
		elif (.url? | type) == "string" then .url
		elif has("url") or has("html_url") then error("invalid comment url")
		else ""
		end;
	[.[] |
		. as $comment |
		($comment | comment_id) as $id |
		($comment | comment_url) as $url |
		($comment.createdAt // $comment.created_at // null) as $raw_timestamp |
		($raw_timestamp | timestamp_key) as $timestamp |
		if ($id == "" or ($comment.body? | type) != "string" or $timestamp == null) then
			error("invalid pull-request comment")
		else
			{id:$id,url:$url,identity:(if $url == "" then $id else $url end),body:$comment.body,created_at:$timestamp,created_at_raw:$raw_timestamp,is_trigger:($comment.body | startswith($prefix))}
		end
	]
' >"$comments_file" 2>/dev/null; then
	uncertain comment-history-invalid "$observed_head"
fi

trigger_count=$(jq '[.[] | select(.is_trigger)] | length' "$comments_file" 2>/dev/null) || uncertain trigger-history-invalid "$observed_head"
if [ "$trigger_count" -eq 0 ]; then
	if [ -n "$request_file" ]; then
		uncertain confirmed-trigger-not-found "$observed_head"
	fi
	emit allow no-trigger "$observed_head" null
	exit 0
fi

trigger_timestamps_ambiguous=$(jq '[.[] | select(.is_trigger) | .created_at] | group_by(.) | any(.[]; length > 1)' "$comments_file" 2>/dev/null) || uncertain trigger-order-invalid "$observed_head"
if [ "$trigger_timestamps_ambiguous" = "true" ]; then
	uncertain trigger-order-ambiguous "$observed_head"
fi
trigger_ids_ambiguous=$(jq '[.[] | select(.is_trigger) | .identity] | group_by(.) | any(.[]; length > 1)' "$comments_file" 2>/dev/null) || uncertain trigger-identity-invalid "$observed_head"
if [ "$trigger_ids_ambiguous" = "true" ]; then
	uncertain trigger-identity-ambiguous "$observed_head"
fi

latest_trigger=$(jq -c '[.[] | select(.is_trigger)] | sort_by(.created_at) | .[-1] | {id:.identity,comment_id:.id,url:.url,created_at:.created_at_raw,order_key:.created_at}' "$comments_file" 2>/dev/null) || uncertain trigger-order-invalid "$observed_head"
latest_trigger_key=$(printf '%s\n' "$latest_trigger" | jq -r '.order_key' 2>/dev/null) || uncertain trigger-order-invalid "$observed_head"
latest_trigger_id=$(printf '%s\n' "$latest_trigger" | jq -r '.id' 2>/dev/null) || uncertain trigger-identity-invalid "$observed_head"
latest_comment_id=$(printf '%s\n' "$latest_trigger" | jq -r '.comment_id' 2>/dev/null) || uncertain trigger-identity-invalid "$observed_head"
latest_trigger_created_at=$(printf '%s\n' "$latest_trigger" | jq -r '.created_at' 2>/dev/null) || uncertain trigger-identity-invalid "$observed_head"

if [ -n "$prior_trigger_id" ] && { [ "$prior_trigger_id" = "$latest_trigger_id" ] || [ "$prior_trigger_id" = "$latest_comment_id" ]; }; then
	if [ "$prior_trigger_created_at" != "$latest_trigger_created_at" ]; then
		uncertain request-identity-mismatch "$observed_head"
	fi
fi

if ! gh api "repos/$repository/pulls/$pull_request/reviews" --paginate >"$reviews_file" 2>/dev/null; then
	uncertain formal-reviews-unavailable "$observed_head"
fi
if ! gh api "repos/$repository/pulls/$pull_request/comments" --paginate >"$inline_file" 2>/dev/null; then
	uncertain inline-comments-unavailable "$observed_head"
fi

reviews_json=$(jq -sc 'if length > 0 and all(.[]; type == "array" and all(.[]; type == "object")) then add else error("formal reviews must be arrays of objects") end' "$reviews_file" 2>/dev/null) || uncertain formal-reviews-invalid "$observed_head"
inline_json=$(jq -sc 'if length > 0 and all(.[]; type == "array" and all(.[]; type == "object")) then add else error("inline comments must be arrays of objects") end' "$inline_file" 2>/dev/null) || uncertain inline-comments-invalid "$observed_head"

if ! printf '%s\n' "$reviews_json" | jq -e '
	def valid_id:
		if (.id? | type) == "string" then (.id | test("^[1-9][0-9]*$"))
		elif (.id? | type) == "number" then (.id | floor == . and . > 0)
		else false
		end;
	def timestamp:
		if (.submitted_at? | type) == "string" then .submitted_at
		elif (.submittedAt? | type) == "string" then .submittedAt
		elif (.created_at? | type) == "string" then .created_at
		elif (.createdAt? | type) == "string" then .createdAt
		else null end;
	def timestamp_key:
		if type != "string" or (test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$") | not) then null
		else capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2})(?<time>T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $parts |
			(try (($parts.base + $parts.time + "Z") | fromdateiso8601) catch null) as $epoch |
			if $epoch == null then null else ($parts.base + $parts.time + "." + (((($parts.fraction // "") + "000000000")[0:9])) + "Z") end
		end;
	all(.[];
		valid_id and
		(.state? | type == "string" and ((ascii_upcase) | IN("PENDING", "COMMENTED", "APPROVED", "CHANGES_REQUESTED", "DISMISSED"))) and
		((timestamp) as $raw | ($raw | timestamp_key) != null)
	)
' >/dev/null 2>&1; then
	uncertain formal-reviews-invalid "$observed_head"
fi

if ! printf '%s\n' "$inline_json" | jq -e '
	def valid_id:
		if (.id? | type) == "string" then (.id | test("^[1-9][0-9]*$"))
		elif (.id? | type) == "number" then (.id | floor == . and . > 0)
		else false
		end;
	def timestamp:
		if (.created_at? | type) == "string" then .created_at
		elif (.createdAt? | type) == "string" then .createdAt
		else null end;
	def timestamp_key:
		if type != "string" or (test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$") | not) then null
		else capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2})(?<time>T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $parts |
			(try (($parts.base + $parts.time + "Z") | fromdateiso8601) catch null) as $epoch |
			if $epoch == null then null else ($parts.base + $parts.time + "." + (((($parts.fraction // "") + "000000000")[0:9])) + "Z") end
		end;
	all(.[];
		valid_id and
		((.body? | type) == "string") and
		((timestamp) as $raw | ($raw | timestamp_key) != null)
	)
' >/dev/null 2>&1; then
	uncertain inline-comments-invalid "$observed_head"
fi

formal_normalized=$(printf '%s\n' "$reviews_json" | jq -c '
	def timestamp:
		if (.submitted_at? | type) == "string" then .submitted_at
		elif (.submittedAt? | type) == "string" then .submittedAt
		elif (.created_at? | type) == "string" then .created_at
		else .createdAt end;
	def timestamp_key:
		capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2})(?<time>T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $parts |
		($parts.base + $parts.time + "." + (((($parts.fraction // "") + "000000000")[0:9])) + "Z");
	[.[] | {id:(.id | tostring),state:(.state | ascii_upcase),created_at:((timestamp) | timestamp_key)}]
' 2>/dev/null) || uncertain formal-reviews-invalid "$observed_head"
inline_normalized=$(printf '%s\n' "$inline_json" | jq -c '
	def timestamp:
		if (.created_at? | type) == "string" then .created_at else .createdAt end;
	def timestamp_key:
		capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2})(?<time>T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $parts |
		($parts.base + $parts.time + "." + (((($parts.fraction // "") + "000000000")[0:9])) + "Z");
	[.[] | {id:(.id | tostring),created_at:((timestamp) | timestamp_key)}]
' 2>/dev/null) || uncertain inline-comments-invalid "$observed_head"

top_equal=$(jq --arg timestamp "$latest_trigger_key" '[.[] | select(.created_at == $timestamp and (.is_trigger | not))] | length' "$comments_file" 2>/dev/null) || uncertain response-order-invalid "$observed_head"
formal_equal=$(printf '%s\n' "$formal_normalized" | jq --arg timestamp "$latest_trigger_key" '[.[] | select(.created_at == $timestamp)] | length' 2>/dev/null) || uncertain response-order-invalid "$observed_head"
inline_equal=$(printf '%s\n' "$inline_normalized" | jq --arg timestamp "$latest_trigger_key" '[.[] | select(.created_at == $timestamp)] | length' 2>/dev/null) || uncertain response-order-invalid "$observed_head"
if [ "$top_equal" -gt 0 ] || [ "$formal_equal" -gt 0 ] || [ "$inline_equal" -gt 0 ]; then
	uncertain response-order-ambiguous "$observed_head"
fi

top_after=$(jq --arg timestamp "$latest_trigger_key" '[.[] | select(.created_at > $timestamp and (.is_trigger | not))] | length' "$comments_file" 2>/dev/null) || uncertain response-history-invalid "$observed_head"
formal_after=$(printf '%s\n' "$formal_normalized" | jq --arg timestamp "$latest_trigger_key" '[.[] | select(.created_at > $timestamp)]' 2>/dev/null) || uncertain response-history-invalid "$observed_head"
inline_after=$(printf '%s\n' "$inline_normalized" | jq --arg timestamp "$latest_trigger_key" '[.[] | select(.created_at > $timestamp)]' 2>/dev/null) || uncertain response-history-invalid "$observed_head"

formal_after_count=$(printf '%s\n' "$formal_after" | jq 'length' 2>/dev/null) || uncertain response-history-invalid "$observed_head"
inline_after_count=$(printf '%s\n' "$inline_after" | jq 'length' 2>/dev/null) || uncertain response-history-invalid "$observed_head"
if [ "$formal_after_count" -gt 0 ] && ! printf '%s\n' "$formal_after" | jq -e 'all(.[]; .state | IN("COMMENTED", "APPROVED", "CHANGES_REQUESTED"))' >/dev/null 2>&1; then
	uncertain formal-response-ambiguous "$observed_head"
fi

if [ "$top_after" -gt 0 ] || [ "$formal_after_count" -gt 0 ] || [ "$inline_after_count" -gt 0 ]; then
	emit allow answered-trigger "$observed_head" "$latest_trigger"
	exit 0
fi

if [ -z "$prior_trigger_id" ]; then
	emit block unanswered-trigger "$observed_head" "$latest_trigger"
	exit 0
fi

if [ "$prior_trigger_id" != "$latest_trigger_id" ] && [ "$prior_trigger_id" != "$latest_comment_id" ]; then
	emit block unanswered-trigger "$observed_head" "$latest_trigger"
	exit 0
fi

if [ "$prior_head_sha" != "$observed_head" ]; then
	emit allow head-changed "$observed_head" "$latest_trigger"
	exit 0
fi

emit block unanswered-trigger "$observed_head" "$latest_trigger"
