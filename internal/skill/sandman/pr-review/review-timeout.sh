#!/bin/sh

# The PR-review skill sources this file from the synced skill tree. Keep the
# arithmetic functions side-effect free so their deadline behavior is easy to
# exercise with fixed timestamps.

review_timeout_now() {
	now=$(date +%s%3N 2>/dev/null) || return 1
	case "$now" in
		''|*[!0-9]*) now=$(date +%s)000 || return 1 ;;
	esac
	printf '%s\n' "$now"
}

review_timeout_integer() {
	case "$1" in
		''|*[!0-9]*) return 1 ;;
	esac
}

review_timeout_deadline() {
	[ "$#" -eq 2 ] || return 2
	review_timeout_integer "$1" || return 2
	review_timeout_integer "$2" || return 2
	printf '%s\n' "$(( $1 + ($2 * 1000) ))"
}

review_timeout_remaining() {
	[ "$#" -eq 2 ] || return 2
	review_timeout_integer "$1" || return 2
	review_timeout_integer "$2" || return 2
	remaining=$(( $1 - $2 ))
	[ "$remaining" -gt 0 ] || return 1
	printf '%s\n' "$remaining"
}

review_timeout_cap_wait() {
	[ "$#" -eq 2 ] || return 2
	review_timeout_integer "$1" || return 2
	review_timeout_integer "$2" || return 2
	[ "$1" -gt 0 ] || return 1
	[ "$2" -gt 0 ] || return 1
	requested_ms=$(( $2 * 1000 ))
	if [ "$requested_ms" -gt "$1" ]; then
		wait_ms=$1
	else
		wait_ms=$requested_ms
	fi
	printf '%s.%03d\n' "$((wait_ms / 1000))" "$((wait_ms % 1000))"
}

review_timeout_state_field() {
	[ "$#" -eq 2 ] || return 2
	[ -f "$1" ] || return 1
	awk -F= -v key="$2" '$1 == key { print substr($0, index($0, "=") + 1); found = 1; exit } END { if (!found) exit 1 }' "$1"
}

review_timeout_state_valid() {
	[ "$#" -eq 1 ] || return 2
	[ -f "$1" ] || return 1
	awk -F= '
		BEGIN { invalid = 0 }
		{
			if (NF != 2) {
				invalid = 1
				next
			}
			key = $1
			if (key != "head_sha" && key != "trigger_id" && key != "started_at" && key != "deadline_at") {
				invalid = 1
				next
			}
			if (seen[key]++ || $2 == "") {
				invalid = 1
			}
			value[key] = $2
		}
		END {
			if (NR != 4 || invalid || !seen["head_sha"] || !seen["trigger_id"] || !seen["started_at"] || !seen["deadline_at"]) {
				exit 2
			}
			if (value["started_at"] !~ /^[0-9]+$/ || value["deadline_at"] !~ /^[0-9]+$/ || value["deadline_at"] + 0 < value["started_at"] + 0) {
				exit 2
			}
		}' "$1"
}

review_timeout_state_matches() {
	[ "$#" -eq 3 ] || return 2
	review_timeout_state_valid "$1" || return $?
	[ "$(review_timeout_state_field "$1" head_sha)" = "$2" ] || return 1
	[ "$(review_timeout_state_field "$1" trigger_id)" = "$3" ] || return 1
	started_at=$(review_timeout_state_field "$1" started_at) || return 1
	deadline_at=$(review_timeout_state_field "$1" deadline_at) || return 1
}

# review_timeout_run runs one direct child under an absolute deadline. It
# returns 124 only when the deadline watcher kills the child or the post-wait
# clock check observes command overhead beyond the deadline.
review_timeout_run() {
	[ "$#" -ge 2 ] || return 2
	deadline=$1
	shift
	now=$(review_timeout_now) || return 125
	remaining=$(review_timeout_remaining "$deadline" "$now") || {
		status=$?
		[ "$status" -eq 1 ] && return 124
		return 125
	}
	review_timeout_cap_wait "$remaining" 1 >/dev/null || return 124

	marker=$(mktemp "${TMPDIR:-/tmp}/sandman-review-timeout.XXXXXX") || return 125
	rm -f "$marker"
	now_before_start=$(review_timeout_now) || {
		rm -f "$marker"
		return 125
	}
	remaining=$(review_timeout_remaining "$deadline" "$now_before_start") || {
		status=$?
		rm -f "$marker"
		[ "$status" -eq 1 ] && return 124
		return 125
	}
	requested_seconds=$(( (remaining + 999) / 1000 ))
	wait_seconds=$(review_timeout_cap_wait "$remaining" "$requested_seconds") || {
		rm -f "$marker"
		return 124
	}
	"$@" &
	child_pid=$!
	now_after_start=$(review_timeout_now) || now_after_start=$deadline
	remaining=$(review_timeout_remaining "$deadline" "$now_after_start") || {
		status=$?
		kill -KILL "$child_pid" 2>/dev/null || true
		wait "$child_pid" 2>/dev/null || true
		rm -f "$marker"
		[ "$status" -eq 1 ] && return 124
		return 125
	}
	requested_seconds=$(( (remaining + 999) / 1000 ))
	wait_seconds=$(review_timeout_cap_wait "$remaining" "$requested_seconds") || {
		kill -KILL "$child_pid" 2>/dev/null || true
		wait "$child_pid" 2>/dev/null || true
		rm -f "$marker"
		return 124
	}
	(
		sleep "$wait_seconds"
		if kill -0 "$child_pid" 2>/dev/null; then
			: >"$marker"
			kill -KILL "$child_pid" 2>/dev/null || true
		fi
	) &
	watcher_pid=$!

	wait "$child_pid"
	child_status=$?

	kill "$watcher_pid" 2>/dev/null || true
	wait "$watcher_pid" 2>/dev/null || true
	now_after=$(review_timeout_now) || now_after=$deadline
	timed_out=false
	if [ -f "$marker" ] || [ "$now_after" -gt "$deadline" ]; then
		timed_out=true
	fi
	rm -f "$marker"
	if [ "$timed_out" = true ]; then
		return 124
	fi
	if [ "$child_status" -eq 124 ]; then
		return 123
	fi
	return "$child_status"
}

review_timeout_write_state() (
	[ "$#" -eq 5 ] || exit 2
	state_path=$1
	head_sha=$2
	trigger_id=$3
	started_at=$4
	deadline_at=$5
	state_dir=$(dirname "$state_path") || exit 125
	mkdir -p "$state_dir" || exit 125
	chmod 700 "$state_dir" || exit 125
	tmp_path=$(mktemp "${state_path}.tmp.XXXXXX") || exit 125
	umask 077
	if ! {
		printf 'head_sha=%s\n' "$head_sha"
		printf 'trigger_id=%s\n' "$trigger_id"
		printf 'started_at=%s\n' "$started_at"
		printf 'deadline_at=%s\n' "$deadline_at"
	} >"$tmp_path"; then
		rm -f "$tmp_path"
		exit 125
	fi
	chmod 600 "$tmp_path" || {
		rm -f "$tmp_path"
		exit 125
	}
	mv -f "$tmp_path" "$state_path" || {
		rm -f "$tmp_path"
		exit 125
	}
)
