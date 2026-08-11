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

response_counts=$(jq -cn \
	--arg trigger_created_at "$trigger_created_at" \
	--arg prefix "$trigger_prefix" \
	--argjson reviews "$reviews_json" \
	--argjson inline "$inline_json" \
	--slurpfile view "$view_file" \
	'{
		top_level: ([($view[0].comments // [])[] | select((.createdAt // .created_at // "") > $trigger_created_at) | select(((.body // "") | startswith($prefix)) | not)] | length),
		formal_reviews: ([$reviews[] | select((.state // "" | ascii_upcase) == "COMMENTED" or (.state // "" | ascii_upcase) == "APPROVED" or (.state // "" | ascii_upcase) == "CHANGES_REQUESTED") | select((.submitted_at // .submittedAt // .created_at // "") > $trigger_created_at)] | length),
		inline_comments: ([$inline[] | select((.created_at // .createdAt // "") > $trigger_created_at)] | length)
	}')

has_response=$(printf '%s' "$response_counts" | jq -r 'if (.top_level + .formal_reviews + .inline_comments) > 0 then "responded" else "pending" end')
jq -cn \
	--arg state "$has_response" \
	--arg observed_head_sha "$observed_head" \
	--arg reason "$has_response" \
	--slurpfile view "$view_file" \
	--argjson reviews "$reviews_json" \
	--argjson inline "$inline_json" \
	--argjson counts "$response_counts" \
	'{state:$state,observed_head_sha:$observed_head_sha,reason:$reason,snapshot:{pull_request_view:$view[0],formal_reviews:$reviews,inline_comments:$inline,response_counts:$counts}}'
