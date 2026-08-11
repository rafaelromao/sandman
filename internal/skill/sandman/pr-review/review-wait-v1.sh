#!/bin/sh

# review-wait-v1 is the portable request-scoped review boundary. The caller
# owns trigger posting and supplies the absolute deadline; this script owns
# request association, polling, and structured result transport.

set -u

request_file=
json_output=false
poll_once=false

usage() {
	cat >&2 <<'EOF'
usage: review-wait-v1.sh --request-file PATH [--json] [--once]
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--request-file)
			[ "$#" -ge 2 ] || { usage; exit 2; }
			request_file=$2
			shift 2
			;;
		--json)
			json_output=true
			shift
			;;
		--once)
			poll_once=true
			shift
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

if [ -z "$request_file" ]; then
	usage
	exit 2
fi

emit_unavailable() {
	reason=$1
	if [ -n "${request_json:-}" ] && jq -e . >/dev/null 2>&1 <<EOF
$request_json
EOF
	then
		jq -cn \
			--arg reason "$reason" \
			--argjson request "$request_json" \
			'{protocol:"review-wait/v1",state:"unavailable",lifecycle:"resumed",request:{repository:$request.repository,pull_request:$request.pull_request,head_sha:$request.head_sha,trigger_id:$request.trigger_id,started_unix_seconds:($request.started_unix_seconds // ($request.deadline_unix_seconds - $request.effective_timeout_seconds)),deadline_unix_seconds:$request.deadline_unix_seconds,effective_timeout_seconds:$request.effective_timeout_seconds},observed_head_sha:"",started_at:$request.started_at,deadline_at:$request.deadline_at,elapsed_seconds:0,reason:$reason,snapshot_path:null,evidence:null}'
	else
		jq -cn --arg reason "$reason" \
			'{protocol:"review-wait/v1",state:"unavailable",lifecycle:"started",request:null,observed_head_sha:"",started_at:"",deadline_at:"",elapsed_seconds:0,reason:$reason,snapshot_path:null,evidence:null}'
	fi
}

if [ ! -f "$request_file" ]; then
	emit_unavailable request-file-missing
	exit 0
fi

request_json=$(jq -c . "$request_file" 2>/dev/null) || {
	emit_unavailable request-file-invalid
	exit 0
}

if ! jq -e '
	.protocol == "review-wait/v1" and
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
	(.deadline_unix_seconds | type == "number" and floor == . and . > 0) and
	(.effective_timeout_seconds | type == "number" and floor == . and . > 0) and
	(.poll_plan | type == "array" and length > 0 and all(.[]; type == "number" and floor == . and . >= 0))
' "$request_file" >/dev/null 2>&1
then
	emit_unavailable request-envelope-invalid
	exit 0
fi

request_started_unix=$(jq -r '(.started_unix_seconds // (.deadline_unix_seconds - .effective_timeout_seconds))' "$request_file")
request_deadline=$(jq -r '.deadline_unix_seconds' "$request_file")
request_timeout=$(jq -r '.effective_timeout_seconds' "$request_file")
if [ "$request_deadline" -ne $((request_started_unix + request_timeout)) ]; then
	emit_unavailable request-envelope-invalid
	exit 0
fi

state_file=$request_file.state
state_json=
lifecycle=started
prior_state=pending

if [ -e "$state_file" ]; then
	state_json=$(jq -c . "$state_file" 2>/dev/null) || {
		emit_unavailable state-file-invalid
		exit 0
	}
	if ! jq -e '
		.protocol == "review-wait/v1" and
		(.repository | type == "string" and length > 0) and
		(.pull_request | type == "number" and floor == . and . > 0) and
		(.head_sha | type == "string" and length > 0) and
		(.trigger_id | type == "string" and length > 0) and
		(.trigger_prefix | type == "string" and length > 0) and
		(.trigger_created_at | type == "string" and length > 0) and
		(.confirmed_at | type == "string" and length > 0) and
		(.effective_timeout_seconds | type == "number" and floor == . and . > 0) and
		(.deadline_unix_seconds | type == "number" and floor == . and . > 0) and
		(.started_at | type == "string" and length > 0) and
		(.deadline_at | type == "string" and length > 0) and
		((.started_unix_seconds // (.deadline_unix_seconds - .effective_timeout_seconds)) | type == "number" and floor == . and . >= 0) and
		((.elapsed_seconds // 0) | type == "number" and floor == . and . >= 0) and
		(.poll_plan == null or (.poll_plan | type == "array" and length > 0 and all(.[]; type == "number" and floor == . and . >= 0))) and
		(.state | IN("pending", "responded", "timed_out", "unavailable"))
	' "$state_file" >/dev/null 2>&1
	then
		emit_unavailable state-file-invalid
		exit 0
	fi

	state_pr=$(jq -r '.pull_request' "$state_file")
	state_repository=$(jq -r '.repository' "$state_file")
	state_head=$(jq -r '.head_sha' "$state_file")
	state_trigger=$(jq -r '.trigger_id' "$state_file")
	state_timeout=$(jq -r '.effective_timeout_seconds' "$state_file")
	state_deadline=$(jq -r '.deadline_unix_seconds' "$state_file")
	request_pr=$(jq -r '.pull_request' "$request_file")
	request_head=$(jq -r '.head_sha' "$request_file")
	request_trigger=$(jq -r '.trigger_id' "$request_file")
	request_timeout=$(jq -r '.effective_timeout_seconds' "$request_file")
	request_deadline=$(jq -r '.deadline_unix_seconds' "$request_file")

	request_repository=$(jq -r '.repository' "$request_file")
	if [ "$state_pr" != "$request_pr" ] || [ "$state_repository" != "$request_repository" ]; then
		emit_unavailable state-request-mismatch
		exit 0
	fi
	if [ "$state_trigger" = "$request_trigger" ]; then
		if [ "$state_head" != "$request_head" ] || [ "$state_timeout" != "$request_timeout" ] || [ "$state_deadline" != "$request_deadline" ]; then
			emit_unavailable same-trigger-request-changed
			exit 0
		fi
		if ! jq -e --argjson request "$request_json" '
			.repository == $request.repository and
			.trigger_prefix == $request.trigger_prefix and
			.trigger_created_at == $request.trigger_created_at and
			.confirmed_at == $request.confirmed_at and
			((.started_unix_seconds // (.deadline_unix_seconds - .effective_timeout_seconds)) == ($request.started_unix_seconds // ($request.deadline_unix_seconds - $request.effective_timeout_seconds))) and
			((.poll_plan // $request.poll_plan) == $request.poll_plan)
		' "$state_file" >/dev/null 2>&1; then
			emit_unavailable same-trigger-request-changed
			exit 0
		fi
		if [ "$(jq -r '.started_at' "$state_file")" != "$(jq -r '.started_at' "$request_file")" ] || [ "$(jq -r '.deadline_at' "$state_file")" != "$(jq -r '.deadline_at' "$request_file")" ]; then
			emit_unavailable same-trigger-timing-changed
			exit 0
		fi
		lifecycle=resumed
		prior_state=$(jq -r '.state' "$state_file")
	else
		# A different confirmed trigger is the only reset boundary. The new
		# request may target the same pull request and head.
		lifecycle=started
		prior_state=pending
	fi
fi

if [ "$prior_state" = "timed_out" ] || [ "$prior_state" = "unavailable" ]; then
	prior_observed_head=$(jq -r '.observed_head_sha // ""' "$state_file")
	prior_elapsed=$(jq -r '.elapsed_seconds // 0' "$state_file")
	jq -cn \
		--argjson request "$request_json" \
		--arg state "$prior_state" \
		--arg lifecycle "$lifecycle" \
		--arg observed_head_sha "$prior_observed_head" \
		--arg reason "$(jq -r '.reason // .state' "$state_file")" \
		--arg snapshot_path "$(jq -r '.snapshot_path // ""' "$state_file")" \
		--argjson elapsed_seconds "$prior_elapsed" \
		--argjson evidence "$(jq -c '.evidence // null' "$state_file")" \
		'{protocol:"review-wait/v1",state:$state,lifecycle:$lifecycle,request:{repository:$request.repository,pull_request:$request.pull_request,head_sha:$request.head_sha,trigger_id:$request.trigger_id,started_unix_seconds:($request.started_unix_seconds // ($request.deadline_unix_seconds - $request.effective_timeout_seconds)),deadline_unix_seconds:$request.deadline_unix_seconds,effective_timeout_seconds:$request.effective_timeout_seconds},observed_head_sha:$observed_head_sha,started_at:$request.started_at,deadline_at:$request.deadline_at,elapsed_seconds:$elapsed_seconds,reason:$reason,snapshot_path:(if $snapshot_path == "" then null else $snapshot_path end),counters:{top:($evidence.response_counts.top_level // 0),reviews:($evidence.response_counts.formal_reviews // 0),inline:($evidence.response_counts.inline_comments // 0)},evidence:$evidence}'
	exit 0
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
observer=${SANDMAN_REVIEW_WAIT_OBSERVER:-$script_dir/review-observe-v1.sh}
clock=${SANDMAN_REVIEW_WAIT_CLOCK:-}
sleeper=${SANDMAN_REVIEW_WAIT_SLEEP:-}
poll_index=0

now() {
	if [ -n "$clock" ]; then
		sh "$clock"
	else
		date +%s
	fi
}

run_observer() {
	observer_output_file=$(mktemp "${TMPDIR:-/tmp}/sandman-review-wait.XXXXXX") || return 125
	observer_group=false
	if command -v setsid >/dev/null 2>&1; then
		setsid sh "$observer" --request-file "$request_file" >"$observer_output_file" 2>/dev/null &
		observer_group=true
	else
		sh "$observer" --request-file "$request_file" >"$observer_output_file" 2>/dev/null &
	fi
	observer_pid=$!
	stop_observer() {
		if [ "$observer_group" = true ]; then
			kill -TERM -"$observer_pid" 2>/dev/null || true
			kill -KILL -"$observer_pid" 2>/dev/null || true
		else
			kill -TERM "$observer_pid" 2>/dev/null || true
			kill -KILL "$observer_pid" 2>/dev/null || true
		fi
	}
	while kill -0 "$observer_pid" 2>/dev/null; do
		watch_now=$(now 2>/dev/null) || {
			stop_observer
			wait "$observer_pid" 2>/dev/null || true
			rm -f "$observer_output_file"
			return 125
		}
		case "$watch_now" in
			''|*[!0-9]*)
				stop_observer
				wait "$observer_pid" 2>/dev/null || true
				rm -f "$observer_output_file"
				return 125
				;;
		 esac
		if [ "$watch_now" -ge "$(jq -r '.deadline_unix_seconds' "$request_file")" ]; then
			stop_observer
			wait "$observer_pid" 2>/dev/null || true
			rm -f "$observer_output_file"
		return 124
		fi
		watch_remaining=$(( $(jq -r '.deadline_unix_seconds' "$request_file") - watch_now ))
		watch_interval=1
		[ "$watch_interval" -le "$watch_remaining" ] || watch_interval=$watch_remaining
		if [ -n "$sleeper" ]; then
			sh "$sleeper" "$watch_interval" >/dev/null 2>&1 || true
		else
			sleep "$watch_interval"
		fi
	done
	wait "$observer_pid"
	observer_status=$?
	if [ "$observer_status" -ne 0 ]; then
		rm -f "$observer_output_file"
		return "$observer_status"
	fi
	cat "$observer_output_file"
	cat_status=$?
	rm -f "$observer_output_file"
	return "$cat_status"
}

write_state() {
	result_state=$1
	result_lifecycle=$2
	result_observed_head=$3
	result_reason=$4
	result_snapshot_path=$5
	result_evidence=$6
	result_elapsed=$7
	state_dir=$(dirname -- "$state_file")
	mkdir -p "$state_dir" || return 1
	state_tmp=$state_file.tmp.$$
	if ! jq -cn \
		--argjson request "$request_json" \
		--arg state "$result_state" \
		--arg lifecycle "$result_lifecycle" \
		--arg observed_head_sha "$result_observed_head" \
		--arg reason "$result_reason" \
		--arg snapshot_path "$result_snapshot_path" \
		--argjson elapsed_seconds "$result_elapsed" \
		--argjson evidence "$result_evidence" \
		'{protocol:"review-wait/v1",repository:$request.repository,pull_request:$request.pull_request,head_sha:$request.head_sha,trigger_id:$request.trigger_id,trigger_prefix:$request.trigger_prefix,trigger_created_at:$request.trigger_created_at,confirmed_at:$request.confirmed_at,started_unix_seconds:($request.started_unix_seconds // ($request.deadline_unix_seconds - $request.effective_timeout_seconds)),effective_timeout_seconds:$request.effective_timeout_seconds,deadline_unix_seconds:$request.deadline_unix_seconds,started_at:$request.started_at,deadline_at:$request.deadline_at,poll_plan:$request.poll_plan,state:$state,lifecycle:$lifecycle,observed_head_sha:$observed_head_sha,elapsed_seconds:$elapsed_seconds,reason:$reason,snapshot_path:(if $snapshot_path == "" then null else $snapshot_path end),evidence:$evidence}' >"$state_tmp"
	then
		rm -f "$state_tmp"
		return 1
	fi
	chmod 600 "$state_tmp" || {
		rm -f "$state_tmp"
		return 1
	}
	mv -f "$state_tmp" "$state_file" || {
		rm -f "$state_tmp"
		return 1
	}
}

emit_result() {
	result_state=$1
	result_lifecycle=$2
	result_observed_head=$3
	result_reason=$4
	result_snapshot_path=$5
	result_evidence=$6
	result_elapsed=$7
	jq -cn \
		--argjson request "$request_json" \
		--arg state "$result_state" \
		--arg lifecycle "$result_lifecycle" \
		--arg observed_head_sha "$result_observed_head" \
		--arg reason "$result_reason" \
		--arg snapshot_path "$result_snapshot_path" \
		--argjson elapsed_seconds "$result_elapsed" \
		--argjson evidence "$result_evidence" \
		'{protocol:"review-wait/v1",state:$state,lifecycle:$lifecycle,request:{repository:$request.repository,pull_request:$request.pull_request,head_sha:$request.head_sha,trigger_id:$request.trigger_id,started_unix_seconds:($request.started_unix_seconds // ($request.deadline_unix_seconds - $request.effective_timeout_seconds)),deadline_unix_seconds:$request.deadline_unix_seconds,effective_timeout_seconds:$request.effective_timeout_seconds},observed_head_sha:$observed_head_sha,started_at:$request.started_at,deadline_at:$request.deadline_at,elapsed_seconds:$elapsed_seconds,reason:$reason,snapshot_path:(if $snapshot_path == "" then null else $snapshot_path end),counters:{top:($evidence.response_counts.top_level // 0),reviews:($evidence.response_counts.formal_reviews // 0),inline:($evidence.response_counts.inline_comments // 0)},evidence:$evidence}'
}

persist_unavailable() {
	result_reason=$1
	result_elapsed=${2:-0}
	if ! write_state unavailable "$lifecycle" "" "$result_reason" "" null "$result_elapsed"; then
		emit_unavailable state-persist-failed
		return 1
	fi
	emit_result unavailable "$lifecycle" "" "$result_reason" "" null "$result_elapsed"
}

while :; do
	now_value=$(now 2>/dev/null) || {
		persist_unavailable clock-unavailable
		exit 0
	}
	case "$now_value" in
		''|*[!0-9]*)
			persist_unavailable clock-invalid
			exit 0
		;;
	esac

	if [ "$now_value" -ge "$(jq -r '.deadline_unix_seconds' "$request_file")" ]; then
		observer_json='{"state":"timed_out","observed_head_sha":"","reason":"request-deadline-exhausted","snapshot":null}'
	else
		observer_output=$(run_observer)
		observer_status=$?
		if [ "$observer_status" -eq 124 ]; then
			observer_json='{"state":"timed_out","observed_head_sha":"","reason":"request-deadline-exhausted","snapshot":null}'
		elif [ "$observer_status" -ne 0 ]; then
			persist_unavailable observer-failed
			exit 0
		else
			observer_json=$(printf '%s' "$observer_output" | jq -c . 2>/dev/null) || {
				persist_unavailable observer-output-invalid
				exit 0
			}
		fi
	fi
	observer_state=$(printf '%s' "$observer_json" | jq -r '.state // ""')
	observer_head=$(printf '%s' "$observer_json" | jq -r '.observed_head_sha // ""')
	observer_reason=$(printf '%s' "$observer_json" | jq -r '.reason // ""')
	observer_evidence=$(printf '%s' "$observer_json" | jq -c '.snapshot // .evidence // null')
	case "$observer_state" in
		responded|timed_out|pending|unavailable) ;;
		*)
			persist_unavailable observer-state-invalid
			exit 0
		;;
	esac
	completed_now=$(now 2>/dev/null) || {
		persist_unavailable clock-unavailable
		exit 0
	}
	case "$completed_now" in
		''|*[!0-9]*)
			persist_unavailable clock-invalid
			exit 0
		;;
	esac
	if [ "$completed_now" -ge "$(jq -r '.deadline_unix_seconds' "$request_file")" ] && [ "$observer_state" != "unavailable" ]; then
		observer_state=timed_out
		observer_reason=request-deadline-exhausted
	fi
	elapsed_seconds=$((completed_now - request_started_unix))
	[ "$elapsed_seconds" -ge 0 ] || elapsed_seconds=0

	snapshot_path=$state_file.snapshot.json
	snapshot_tmp=$snapshot_path.tmp.$$
	if ! printf '%s\n' "$observer_json" >"$snapshot_tmp" || ! chmod 600 "$snapshot_tmp" || ! mv -f "$snapshot_tmp" "$snapshot_path"; then
		rm -f "$snapshot_tmp"
		persist_unavailable snapshot-persist-failed
		exit 0
	fi
	decision_now=$(now 2>/dev/null) || {
		persist_unavailable clock-unavailable
		exit 0
	}
	case "$decision_now" in
		''|*[!0-9]*)
			persist_unavailable clock-invalid
			exit 0
		;;
	esac
	if [ "$decision_now" -ge "$(jq -r '.deadline_unix_seconds' "$request_file")" ] && [ "$observer_state" != "unavailable" ]; then
		observer_state=timed_out
		observer_reason=request-deadline-exhausted
	fi
	elapsed_seconds=$((decision_now - request_started_unix))
	[ "$elapsed_seconds" -ge 0 ] || elapsed_seconds=0

	result_reason=$observer_reason
	[ -n "$result_reason" ] || result_reason=$observer_state
	if [ "$observer_state" = "responded" ] || [ "$observer_state" = "unavailable" ] || [ "$observer_state" = "timed_out" ]; then
		if ! write_state "$observer_state" "$lifecycle" "$observer_head" "$result_reason" "$snapshot_path" "$observer_evidence" "$elapsed_seconds"; then
			emit_unavailable state-persist-failed
			exit 0
		fi
		emit_result "$observer_state" "$lifecycle" "$observer_head" "$result_reason" "$snapshot_path" "$observer_evidence" "$elapsed_seconds"
		exit 0
	fi

	if [ "$poll_once" = true ]; then
		if ! write_state pending "$lifecycle" "$observer_head" pending "$snapshot_path" "$observer_evidence" "$elapsed_seconds"; then
			emit_unavailable state-persist-failed
			exit 0
		fi
		emit_result pending "$lifecycle" "$observer_head" pending "$snapshot_path" "$observer_evidence" "$elapsed_seconds"
		exit 0
	fi

	if [ "$decision_now" -ge "$(jq -r '.deadline_unix_seconds' "$request_file")" ]; then
		if ! write_state timed_out "$lifecycle" "$observer_head" request-deadline-exhausted "$snapshot_path" "$observer_evidence" "$elapsed_seconds"; then
			emit_unavailable state-persist-failed
			exit 0
		fi
		emit_result timed_out "$lifecycle" "$observer_head" request-deadline-exhausted "$snapshot_path" "$observer_evidence" "$elapsed_seconds"
		exit 0
	fi

	interval=$(jq -r --argjson index "$poll_index" 'if $index < (.poll_plan | length) then .poll_plan[$index] else .poll_plan[-1] end' "$request_file")
	remaining=$(( $(jq -r '.deadline_unix_seconds' "$request_file") - decision_now ))
	sleep_interval=$interval
	[ "$sleep_interval" -le "$remaining" ] || sleep_interval=$remaining
	if [ -n "$sleeper" ]; then
		sh "$sleeper" "$sleep_interval" || {
			persist_unavailable sleeper-failed
			exit 0
		}
	else
		sleep "$sleep_interval" || {
			persist_unavailable sleeper-failed
		exit 0
		}
	fi
	poll_index=$((poll_index + 1))
done
