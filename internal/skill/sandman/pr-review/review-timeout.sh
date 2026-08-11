#!/bin/sh

# The PR-review skill sources this file from the synced skill tree. Keep the
# arithmetic functions side-effect free so their deadline behavior is easy to
# exercise with fixed timestamps.

review_timeout_now() {
	date +%s
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
	printf '%s\n' "$(( $1 + $2 ))"
}

review_timeout_remaining() {
	[ "$#" -eq 2 ] || return 2
	review_timeout_integer "$1" || return 2
	review_timeout_integer "$2" || return 2
	printf '%s\n' "$(( $1 - $2 ))"
}

review_timeout_cap_wait() {
	[ "$#" -eq 2 ] || return 2
	review_timeout_integer "$1" || return 2
	review_timeout_integer "$2" || return 2
	[ "$1" -gt 0 ] || return 1
	[ "$2" -gt 0 ] || return 1
	if [ "$2" -gt "$1" ]; then
		printf '%s\n' "$1"
	else
		printf '%s\n' "$2"
	fi
}

# review_timeout_run runs one direct child under an absolute deadline. It
# returns 124 only when the deadline watcher kills the child or the post-wait
# clock check observes command overhead beyond the deadline.
review_timeout_run() {
	[ "$#" -ge 2 ] || return 2
	deadline=$1
	shift
	now=$(review_timeout_now) || return 125
	remaining=$(review_timeout_remaining "$deadline" "$now") || return 125
	wait_seconds=$(review_timeout_cap_wait "$remaining" "$remaining") || return 124

	marker=$(mktemp "${TMPDIR:-/tmp}/sandman-review-timeout.XXXXXX") || return 125
	rm -f "$marker"
	"$@" &
	child_pid=$!
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
