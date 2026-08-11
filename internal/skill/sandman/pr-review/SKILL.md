---
name: sandman-pr-review
description: Automates the GitHub PR review loop with the PR Review Agent. Waits for CI to pass, requests review from the PR Review Agent by posting "{{REVIEW_COMMAND}}" on the PR, then polls for feedback, applies suggestions, commits, pushes, and repeats until approved or max 10 passes. Use when user says sandman pr-review, wants a PR reviewed iteratively by the PR Review Agent, wants to auto-address review feedback, or mentions review loop, {{REVIEW_COMMAND}}, or iterative PR approval.
---

# PR Review

## Hard rules

1. **You must NOT review the PR yourself in this session.**
   Your only job is to delegate the review to the PR Review Agent by posting `{{REVIEW_COMMAND}}` as a PR comment, then wait for the PR Review Agent's feedback and act on it. Under no circumstances should you read the diff and provide your own review comments.

2. **You must NOT finish on ambiguous feedback.** If the reviewer's intent cannot be reduced to a concrete, actionable code change, do not guess, do not change code, and do not stop the loop. Post a new PR comment that includes `{{REVIEW_COMMAND}}` plus a freeform request asking the reviewer to clarify the intended actionable change, then continue polling. The loop only ends on approval (formal case A or informal case C), an explicit skill-defined irrecoverable stop condition, or max passes reached — never on ambiguity.

3. **You must NOT finish before the review timeout or max attempts when no feedback has been provided.** If `reviewDecision` is still `REVIEW_REQUIRED` (or absent), no reviews exist yet, no inline file comments exist, and only boilerplate setup comments are present, keep polling. Do not declare done or stop the loop. The only acceptable reasons to exit early are: approval (formal case A or informal case C), an explicit skill-defined irrecoverable stop condition, or 10 passes reached.

4. **You must NOT exit the polling loop on a `0/0` count of (formal reviews, inline comments) when the top-level PR conversation has a new non-trigger comment.** A reviewer who only posts a top-level PR conversation comment (no formal review event, no inline file comments) is still a real reviewer response. Re-classify the state, run the self-check (Step 4), and continue polling — do not give up.

5. **You must NOT request another review while a previous `{{REVIEW_COMMAND}}` is still waiting for a response AND the PR head SHA has not changed.** Only post `{{REVIEW_COMMAND}}` again after either: (a) the reviewer has responded to the previous request, OR (b) a new commit has landed on the PR branch (head SHA changed). If the SHA changed, the previous request is stale — re-request regardless of feedback state. If SHA is unchanged but a response arrived, act on it before re-requesting.

6. **You must NOT request another review before the previous one has produced a response, UNLESS a new commit has landed.** Every iteration that would post a new `{{REVIEW_COMMAND}}` must first check whether the head SHA has changed since the last request. If SHA changed, treat the previous request as consumed and allow re-requesting. If SHA is unchanged, only re-request after a response has arrived.

7. **You must NOT request review until CI is green.** If CI is still pending or failing, keep polling Step 2 and do not post `{{REVIEW_COMMAND}}` yet.

8. **You must NOT give up on a `CHANGES_REQUESTED` review when the reviewer's request maps to the issue description or acceptance criteria.** When the reviewer flags a requirement that comes from the issue body or its acceptance criteria (the same criteria the implementor agent was asked to satisfy), you have exactly two acceptable paths:
   - **Implement the requested change.** Read the issue description and its acceptance criteria, confirm the reviewer's interpretation is consistent with them, then make the change, commit, push, and re-request review.
   - **Convince the reviewer the requirement is out of scope.** Post a PR comment that quotes the issue's own acceptance criteria verbatim, explains why the requested change falls outside the issue's stated scope, and asks the reviewer to either accept the narrowed scope or correct the implementor's interpretation. Then **wait for the reviewer's explicit agreement** before considering the `CHANGES_REQUESTED` resolved. If the reviewer reaffirms the change is required, you must implement it on the next pass — you cannot keep asserting your own interpretation against theirs.
   
   It is NEVER acceptable to assert "this is out of scope" unilaterally and exit the loop with a `CHANGES_REQUESTED` still pending. If max passes are reached with the deadlock unresolved, exit the loop with a clearly-documented `CHANGES_REQUESTED_UNRESOLVED` reason in `.sandman/task.md` and the run log so the failure and next executable action are durable — do not silently terminate as if the work were complete.

9. **Any PR comment intended to be read by the reviewer MUST start with the review command.** A comment that does not begin with the review command is treated as boilerplate by the daemon and ignored — it does not reach the reviewer and does not advance the loop. Concretely:
    - When posting the trigger comment (Step 4), the body must be exactly the review command on its own (e.g. via the platform's "post change-request comment" CLI, passing the change-request identifier and the review-command body).
    - When posting a clarification request, a follow-up after a stalled poll, or any other reviewer-facing message, the body must begin with the review command and may include additional freeform text afterwards (e.g. `{{REVIEW_COMMAND}} — please clarify which file you mean`). The leading review-command substring is what the daemon's trigger filter matches on; the trailing freeform text is read by the reviewer but ignored by the trigger filter.
    - When posting the bot's own review-body, do NOT prefix it with the review command. The review-body is the substance the reviewer writes back to you — prefixing it would cause the daemon to mis-classify the body as a duplicate trigger on the next tick and drop the actual review content.

10. **You must NOT dismiss a review-shaped response based on who posted it, and you must NOT investigate how the reviewer is hosted.** Any comment that arrives after your review request and contains review content — a summary, findings, an approval, change requests, or substantive feedback on the diff — is a valid reviewer response. Accept it, classify it per Step 6, and act on it. Do not filter it out because the author shares your GitHub login, because `viewerDidAuthor` is true, or because you cannot identify a separate reviewer account. Do not inspect `.github/workflows/`, branch protection rules, collaborator lists, or any repository configuration to determine who will respond to the review request — the reviewer may be a local process, a CI action, a bot, a separate user, or the same operator under the same credentials. The skill does not assume any of these, and neither should you.

## Workflow

### Prerequisites

- `gh` CLI authenticated with repo access
- PR is already open, branch is pushed
- Working directory at the repo root

### State tracked across passes

- `.sandman/state/<N>.head_sha` — the head commit SHA at which the last `{{REVIEW_COMMAND}}` was posted. If the current head SHA differs, all previous review state is stale and a fresh request is always permitted.
- `.sandman/state/<N>.addressed_comments` — one inline comment ID per line, tracking which inline comments have already been acted on. Cleared when head SHA changes (new commit invalidates all old inline comment IDs).
- `.sandman/state/<N>.review_request.json` — the atomic, confirmed request envelope containing the pull request, head SHA, trigger identity, start, deadline, budget, and poll plan.
- `.sandman/state/<N>.review_request.json.state` — the atomic wait result for that request. The request envelope and its matching head-SHA sidecar must both be trusted before re-entry.

### Iteration loop (max 10 passes)

#### Step 1: Get current PR state

```bash
pr_data=$(gh pr view <N> --repo <owner/repo> --json headRefOid,comments,reviewDecision,mergeStateStatus)
mergeStateStatus=$(echo "$pr_data" | jq -r '.mergeStateStatus')
headRefOid=$(echo "$pr_data" | jq -r '.headRefOid')
head_sha="$headRefOid"
reviewDecision=$(echo "$pr_data" | jq -r '.reviewDecision')
comments=$(echo "$pr_data" | jq -r '.comments')
```

#### Step 2: Wait for CI to pass

The CI wait has a 60-minute budget per PR head SHA. A failed check gets at most 3 fix-and-push attempts for that SHA; after the budget or attempts are exhausted, record `CI_TIMEOUT` or `CI_FAILURE_UNRESOLVED` in `.sandman/task.md` and the run log with the exact failure and next executable action, then leave the PR open for the next run.

Enforce those limits in the polling loop with a deadline and attempt counter:

```bash
ci_deadline=$(( $(date +%s) + 3600 ))
ci_fix_attempts=0
```

Before each CI poll, compare the current time with `ci_deadline`. On a failed check, if `ci_fix_attempts` is already 3, record `CI_FAILURE_UNRESOLVED` and exit the review attempt; otherwise increment `ci_fix_attempts` before applying the fix and pushing. When the deadline is reached, record `CI_TIMEOUT` and exit the review attempt. A new head SHA starts a fresh deadline and counter.

> **Prerequisite**: `gh` ≥ 2.0 (released 2021) for `gh pr checks --json ... --jq`. Verify with `gh --version | awk '{print $1, $3}'` before relying on the loop. On older `gh` the `--json` flag is unknown; fall back to plain `gh pr checks <N> --repo <owner/repo>` and parse the first column instead.

```bash
# Step 2 must wait for CI, but CI cannot run on a DIRTY (conflicting) PR.
# Step 1 already fetched mergeStateStatus — use it directly. If DIRTY, trigger back-merge first.
if [ "$mergeStateStatus" = "DIRTY" ]; then
  echo "PR is in 'DIRTY' state (merge conflicts). CI cannot run. Running sandman-back-merge to resolve."
  # Load and run back-merge: merges the base branch into the current branch, resolves conflicts, pushes.
  # This can recover fixes that landed on main after the branch was created.
  # If back-merge fails, the PR remains unmergeable — keep polling so we re-enter this block.
  if sandman-back-merge; then
    echo "Back-merge succeeded, pushing and re-checking CI."
    git push
    continue  # restart CI wait loop after push triggers new CI run
  else
    echo "Back-merge failed or unresolved conflicts — CI still blocked. Continuing to poll."
    sleep 20
    continue
  fi
fi

# gh pr checks --json returns state values in uppercase:
#   SUCCESS, FAILURE, PENDING, IN_PROGRESS, QUEUED, NEUTRAL,
#   CANCELLED, TIMED_OUT, ACTION_REQUIRED, STARTUP_FAILURE, STALE, SKIPPED.
# We classify each state into "fail", "pending", or "pass" and loop until
# no "pending" remains (with "fail" taking priority).
while true; do
  states=$(gh pr checks <N> --repo <owner/repo> --json name,state \
    --jq '.[] | select(.state != "SKIPPED") | .state' 2>/dev/null)
  if [ -z "$states" ]; then sleep 20; continue; fi
  # Fail: read logs, fix, push, then continue waiting for the new CI run.
  if echo "$states" | grep -qE '^(FAILURE|STARTUP_FAILURE|TIMED_OUT|ACTION_REQUIRED|CANCELLED)$'; then
    echo "CI failed:"; gh pr checks <N> --repo <owner/repo>
    # Fetch failure reason from job logs.
    job_id=$(gh api repos/<owner>/<repo>/actions/runs \
      --jq '.workflow_runs[0].jobs[] | select(.conclusion == "failure") | .id' 2>/dev/null)
    if [ -n "$job_id" ]; then
      gh api repos/<owner>/<repo>/actions/jobs/<job_id>/logs \
        --jq '.text' 2>/dev/null | tail -50
    fi
    # If the failure looks like base-branch drift, use sandman-back-merge to pull in fixes before retrying.
    # This can recover fixes that landed after the task started.
    # Fix it. Read relevant source files, make minimal changes.
    git add -A && git commit -m "fix: resolve CI failure on <N>" && git push
    # After pushing, the old CI run is irrelevant.
    # Continue to wait for the NEW CI run triggered by the push.
    continue
  fi
  # Pending: keep waiting.
  if echo "$states" | grep -qE '^(PENDING|IN_PROGRESS|QUEUED)$'; then
    sleep 20; continue
  fi
  # All remaining states are terminal non-fail (SUCCESS, NEUTRAL, STALE).
  break
done
```

On CI failure, `continue` the outer loop to wait for the newly-triggered CI run after pushing the fix — not `exit 1`, which would fall through to posting a review on a broken PR. If `mergeStateStatus` is `DIRTY`/`CONFLICTING` (the PR has merge conflicts), CI cannot run at all: Step 2 detects that upfront and delegates to `sandman-back-merge` to resolve the conflict before waiting for CI. This prevents the agent from spinning forever on empty check results or declaring the PR "requires manual resolution."

#### Step 3: Check if SHA changed (stale request check)

Before using `.sandman/state/<N>.head_sha`, validate the confirmed request pair. The request envelope is the authoritative identity record; the head-SHA sidecar is only a derived compatibility record for stale-approval rules:

```bash
request_file=".sandman/state/<N>.review_request.json"
head_file=".sandman/state/<N>.head_sha"
if [ -e "$request_file" ] || [ -e "$head_file" ]; then
  [ -f "$request_file" ] && [ -f "$head_file" ] || record REVIEW_TIMEOUT_STATE_ERROR and stop
  jq -e --arg repository "<owner/repo>" --argjson pull_request <N> '
    .protocol == "review-wait/v1" and
    .repository == $repository and .pull_request == $pull_request and
    (.head_sha | type == "string" and length > 0) and
    (.trigger_id | type == "string" and length > 0) and
    (.trigger_created_at | type == "string" and length > 0) and
    (.confirmed_at | type == "string" and length > 0) and
    (.started_at | type == "string" and length > 0) and
    (.deadline_at | type == "string" and length > 0) and
    (.effective_timeout_seconds | type == "number" and floor == . and . > 0) and
    (.deadline_unix_seconds == ((.started_unix_seconds // (.deadline_unix_seconds - .effective_timeout_seconds)) + .effective_timeout_seconds)) and
    ((.started_unix_seconds // (.deadline_unix_seconds - .effective_timeout_seconds)) >= 0)
  ' "$request_file" >/dev/null 2>&1 || record REVIEW_TIMEOUT_STATE_ERROR and stop
  persisted_head_sha=$(jq -er '.head_sha' "$request_file") || record REVIEW_TIMEOUT_STATE_ERROR and stop
  recorded_head_sha=$(tr -d '\r\n' <"$head_file") || record REVIEW_TIMEOUT_STATE_ERROR and stop
  [ "$persisted_head_sha" = "$recorded_head_sha" ] || record REVIEW_TIMEOUT_STATE_ERROR and stop
fi
```

If either artifact is missing, malformed, or mismatched, fail closed. Do not
silently repair it by posting another trigger or by calculating a new deadline.
Only a fully trusted pair may be re-entered.

Read the trusted `.sandman/state/<N>.head_sha` if it exists and compare it
against the current head SHA from Step 1. If neither request artifact exists,
this is the first request and no deadline exists yet.

- **SHA changed** (new commit landed since last request): mark all previous review state stale. Delete `.sandman/state/<N>.addressed_comments` if it exists, because inline comment IDs from the old commit are no longer relevant. A fresh review request is always permitted. The pass counter resets to 0 — a new commit is a new review cycle on a new diff, and any exhausted 10-pass budget against the prior SHA does not apply. **All prior approvals are stale** for the purposes of Step 6 Case C — the new SHA requires a fresh APPROVED comment (formal or informal) against the new diff, not a re-application of an approval that was issued against the prior SHA. This is the symmetric counterpart of the pass-counter reset: if the budget must reset, the approval window must reset too.
- **SHA unchanged**: apply the "previous request still pending" logic before posting again.

#### Step 4: Delegate review to the PR Review Agent (trigger post)

If SHA changed since the last request, always allow re-requesting. If SHA is unchanged, skip this step if no review response has arrived yet.

Only post `{{REVIEW_COMMAND}}` after CI has reached a green terminal state in Step 2.

The post result is not a request until the trigger is confirmed against the
current PR head. Re-read the PR comments and capture the exact server
`createdAt` for the returned comment ID:

```bash
trigger_url=$(gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}") || record REVIEW_TIMEOUT_STATE_ERROR and stop
trigger_id="$trigger_url"
trigger_created_at=$(gh pr view <N> --repo <owner/repo> --json headRefOid,comments |
  jq -er --arg head "$head_sha" --arg trigger_url "$trigger_url" --arg prefix "{{REVIEW_COMMAND}}" '
    select(.headRefOid == $head) |
    first(.comments[] | select((.url // "") == $trigger_url) |
      select((.body // "") | startswith($prefix))) | .createdAt
  ') || record REVIEW_TIMEOUT_STATE_ERROR and stop
```

Only after this confirmation, atomically write the request envelope used by the
versioned wait. The caller supplies the absolute deadline and polling plan;
the compatibility form below materializes those fields from the already
resolved effective timeout. It does not choose defaults, minimums, or a
pull-request-wide budget:

```bash
review_timeout=${REVIEW_TIMEOUT:-1800}
request_started_unix=$(date +%s) || record REVIEW_TIMEOUT_STATE_ERROR and stop
request_deadline_unix=$((request_started_unix + review_timeout))
request_started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ) || record REVIEW_TIMEOUT_STATE_ERROR and stop
request_file=".sandman/state/<N>.review_request.json"
mkdir -p "${request_file%/*}" || record REVIEW_TIMEOUT_STATE_ERROR and stop
request_tmp=$(mktemp "${request_file}.tmp.XXXXXX") || record REVIEW_TIMEOUT_STATE_ERROR and stop
jq -n \
  --arg repository "<owner/repo>" \
  --arg head_sha "$head_sha" \
  --arg trigger_id "$trigger_id" \
  --arg trigger_prefix "{{REVIEW_COMMAND}}" \
  --arg trigger_created_at "$trigger_created_at" \
  --arg confirmed_at "$request_started_at" \
  --arg started_at "$request_started_at" \
  --arg deadline_at "unix:$request_deadline_unix" \
  --argjson pull_request <N> \
  --argjson started_unix_seconds "$request_started_unix" \
  --argjson deadline_unix_seconds "$request_deadline_unix" \
  --argjson effective_timeout_seconds "$review_timeout" \
  --argjson poll_plan '[120,60,60,30]' \
  '{protocol:"review-wait/v1",repository:$repository,pull_request:$pull_request,
    head_sha:$head_sha,trigger_id:$trigger_id,trigger_prefix:$trigger_prefix,
    trigger_created_at:$trigger_created_at,confirmed_at:$confirmed_at,
    started_unix_seconds:$started_unix_seconds,
    started_at:$started_at,deadline_at:$deadline_at,
    deadline_unix_seconds:$deadline_unix_seconds,
    effective_timeout_seconds:$effective_timeout_seconds,poll_plan:$poll_plan}' \
  >"$request_tmp" && mv -f "$request_tmp" "$request_file" || {
    rm -f "$request_tmp"
    record REVIEW_TIMEOUT_STATE_ERROR and stop
  }
head_tmp=$(mktemp ".sandman/state/<N>.head_sha.tmp.XXXXXX") || record REVIEW_TIMEOUT_STATE_ERROR and stop
printf '%s\n' "$head_sha" >"$head_tmp" && mv -f "$head_tmp" ".sandman/state/<N>.head_sha" || {
  rm -f "$head_tmp"
  record REVIEW_TIMEOUT_STATE_ERROR and stop
}

The request envelope is the atomic request record. The atomic head-SHA sidecar
remains available to the existing stale-approval rules and subsequent passes,
but a failure to persist either artifact is a state error. A later continuation
must fail closed on that pair rather than silently reposting or resetting.

#### Step 5: Wait for this confirmed request (versioned coordinator)

The effective `REVIEW_TIMEOUT` value is an integer number of seconds supplied by
the current AgentRun task context, not a value synchronized into this globally
installed skill. If older task context has no value, use the compatibility
fallback of **1800 seconds**. The request envelope fixes that value, its
absolute deadline, and the existing polling plan for one confirmed trigger.
Retries and continuations invoke the same request again; they do not create a
new deadline. A later confirmed trigger replaces the request envelope and is a
new request, even when the pull request and head are unchanged.

Invoke the installed coordinator exactly once for this wait. It is a portable
POSIX-shell entry point shipped with the shared skill, so it works in a
worktree and in a container where the skill is visible at `/.agents`; it does
not require a host `sandman` binary. It does not require a host `sandman` binary:

```bash
skill_root="${SANDMAN_SKILL_ROOT:-${HOME}/.agents/skills/sandman}"
wait_result=$(sh "$skill_root/pr-review/review-wait-v1.sh" \
  --request-file "$request_file" --json) || record REVIEW_TIMEOUT_STATE_ERROR and stop
wait_state=$(printf '%s' "$wait_result" | jq -r '.state // "unavailable"')
wait_reason=$(printf '%s' "$wait_result" | jq -r '.reason // "unknown"')
```

The result is one structured request-scoped envelope. `responded` means that
raw evidence is available for the existing Step 6 classifier and that the
request-scoped classification is present; it does not mean approval. The raw
evidence includes the complete top-level, formal-review, and inline-comment
snapshot plus `top`, `reviews`, and `inline` counters. The additive
`classification` object is the only decision input: it records the confirmed
request identity, the current head, the active-to-next-trigger window, filtered
source arrays, canonical event timestamps, head status, and formal precedence.
Its `boundary_evidence` section carries the same request identity, deadline,
and source timestamps for an exact-deadline decision.
Handle the transport state before classification:

- `responded`: validate `classification.protocol == "review-classification/v1"`,
  then read its request-scoped sources and
  continue to Step 6 without adding response formats or author filters. The
  raw snapshot is audit evidence only; if classification is missing or
  malformed, fail closed as `unavailable`.
- `pending`: no eligible response was observed and the request remains active;
  re-enter this same request, never post a duplicate trigger.
- `timed_out`: record `REVIEW_TIMEOUT` with the request identity, configured
  budget, deadline, `elapsed_seconds`, counters, and next executable action. Do
  not approve or create a replacement request.
- `unavailable`: record the structured reason and fail closed. Do not approve,
  reset the deadline, or post another trigger to recover it.

The coordinator preserves the existing `120, 60, 60, then 30` poll plan and
observed-response fast path as data. It owns the polling control and deadline
boundary for this request; the skill no longer tracks cumulative sleep or
open-codes the three GitHub response calls. A new request starts a fresh plan;
ordinary polls, pending-trigger checks, retries, and continuations reuse the
same request. The first intervals remain 120 seconds, 60 seconds, and 60
seconds; later intervals remain 30 seconds. The existing 240-second minimum
and boundary rule are unchanged: every operation uses the remaining absolute wall-clock budget, a final interval is shortened to that remainder, and sleep that lands exactly on the configured budget is permitted. A new review request starts a fresh counter; it also receives a full deadline, while the same request retains its original start/deadline and observed response counts.

The response snapshot retains the existing source contract: author-agnostic
top-level non-trigger comments, formal `COMMENTED`/`APPROVED`/
`CHANGES_REQUESTED` reviews, and inline file comments. The current
same-credential, stale-head, pending-trigger, requested-changes, informal
approval, and concrete-feedback rules remain owned by Step 6. The bundled
`review-observe-v1.sh` observer returns a response whose body does not begin with `{{REVIEW_COMMAND}}` regardless of author; it excludes a top-level response only when that body begins with the configured prefix. Do not filter by author. Preserve observed response counts, and use only the active request's window in the classification.
An envelope with `state:"unavailable"` is structured failure, never approval.

Before Step 6, retain the existing self-check: when `top > 0`, `reviews == 0`,
and `inline == 0`, and no previous `{{REVIEW_COMMAND}}` request is already
pending, post a follow-up beginning with `{{REVIEW_COMMAND}}` asking the
reviewer to clarify. If a request is already pending, do not pile on another
trigger. This is reviewer communication, not a new response classification.

#### Step 5a: DIRTY handling — every coordinator result

> **Prerequisite**: a DIRTY (`mergeable == CONFLICTING`) PR cannot run CI, cannot be reviewed on its diff cleanly, and cannot be merged. The Step 2 pre-check catches the initial state, but a PR can drift to DIRTY while the coordinator is waiting. This section remains the per-result guard.

On every `responded` or `pending` result, inspect the `mergeStateStatus` field
in the returned snapshot (do **not** make a separate change-request view call).
If `mergeStateStatus == "DIRTY"`:

1. Stop polling for review feedback. The PR is unmergeable until the conflict is resolved; reviewer comments posted on a DIRTY PR do not produce a usable review.
2. Load `sandman-back-merge` (see the `sandman-back-merge` skill). Run it on the current branch. It performs the disciplined 3-way merge of the base branch into the working branch and resolves conflicts without history rewrites.
3. If back-merge succeeds, push the updated branch with `git push`. Update `.sandman/state/<N>.head_sha` with the new head SHA so Step 3's stale-request check sees the new commit and re-evaluates.
4. Restart from Step 1 — a fresh CI run will be triggered by the push, and the review agent may have already posted feedback on the prior SHA that the next request result should classify.
5. If back-merge fails to resolve conflicts (e.g. semantic conflict, merge helper rejected a hunk), exit the loop with a distinct `REVIEW_CONFLICT_UNRESOLVED` reason in `.sandman/task.md` and the run log. This is **never** a `REVIEW_TIMEOUT`. It is also **never** a silent success — the PR remains unmergeable and a future run must continue from this state.

**Hard rule — DIRTY is not REVIEW_TIMEOUT.** A DIRTY PR that back-merge cannot resolve is a structured failure with a downstream signal in the run payload. Do not collapse it into the generic review-timeout bucket: the two failures have different remediation paths and different downstream tooling.

**Hard rule — DIRTY is not silent success.** Observing a DIRTY PR and continuing to poll for reviewer comments is the failure mode the skill exists to prevent. The fix is action, not observation.

#### Step 6: Read and classify feedback

**A. Formal approval detected?**
- `classification.formal.decision == "approved"`, with a non-empty
  `classification.formal.approval_evidence` containing current-head formal
  `APPROVED` records
→ **Approve** — done, exit the loop and document the approval in the run log.

The pull-request-wide `reviewDecision` aggregate and the unfiltered formal
review array are audit evidence only. They cannot create request-scoped
approval, especially when an approval has a stale or unknown commit identity.

**B. Formal changes requested?**
- `classification.formal.requested_changes` contains any formal
  `CHANGES_REQUESTED` record in the active request window. This includes a
  record whose explicit commit is stale, preserving requested-changes
  precedence over unrelated approval evidence.
→ **Blockers** — must fix before continuing. Apply Hard Rule 7 (issue ACs): if the reviewer's request maps to a requirement from the issue body or acceptance criteria, you must either implement the change or get the reviewer's explicit agreement that the scope is narrower. Posting a "this is out of scope" comment and exiting the loop is NOT an acceptable resolution — it leaves the `CHANGES_REQUESTED` pending and the PR unmerged.

**C. Informal approval (implicit approval without formal review)?**
- `classification.request_state == "active"`, AND
- No pending `classification.formal.requested_changes`, AND
- A request-scoped `COMMENTED` review OR top-level comment with approval keywords, AND
- The source is not marked `head_status: "stale"` or `"unknown"`, AND
- **The approval is for the current diff** — its `createdAt` is **after** the SHA recorded in `.sandman/state/<N>.head_sha` (the SHA at which the most recent `{{REVIEW_COMMAND}}` was posted), AND
- **No unanswered `/sandman review` trigger is sitting above the approval** — the most recent top-level comment by `createdAt` is not an implementor trigger that has not yet received a response, AND
- **The minimum polling cycle has elapsed** — at least 240 s of cumulative sleep (one full `120 + 60 + 60` cycle from Step 5) has passed since the most recent trigger post. A single 120 s first poll cannot have observed a meaningful response window.
→ **Approve — DONE. Stop the loop.** An informal approval is sufficient.

Approval keywords: `lgtm`, `looks good`, `looks great`, `nice work`, `good work`, `approved`, `ship it`, `+1`, `thumbs up`, `all good`, `all set`, `good to go`, `no major issues`, `minor issues only`, etc.

**Hard rule — stale approval after a SHA change.** When Step 3 detects that the head SHA has changed since the last `{{REVIEW_COMMAND}}` post, every prior approval timestamp is stale. The new diff requires a fresh approval against the new SHA, not a re-application of an approval that was issued against the prior diff. Treat any pre-SHA-change APPROVED comment as **not approved** until a new APPROVED comment arrives after the new SHA — even if `reviewDecision` would otherwise be empty and the comment list otherwise looks clean. The pass-counter reset on SHA change implies the approval window resets too; do not infer that a stale APPROVED carries forward across a SHA change.

**Hard rule — pending trigger beats older approval.** When the most recent top-level comment on the PR is an implementor `{{REVIEW_COMMAND}}` trigger that has not yet received a response, do not classify an older APPROVED comment below it as Case C. The trigger is itself a fresh request; the loop must continue polling until either the trigger receives a response or the confirmed request's absolute `REVIEW_TIMEOUT` deadline is exhausted. This is the symmetric counterpart of Hard Rule 5 / Hard Rule 6 (which prevent *posting* a duplicate trigger before the previous one is answered) and closes the same race at the *exit* side of the loop.

The structured equivalent is `classification.request_state == "superseded"`:
an older request may retain its response evidence for audit, but its decision is
`pending` and it cannot approve. Only the later confirmed request may classify
responses in the interval after its own trigger.

**Hard rule — minimum poll budget before Case C.** Even when the other Case C gates (no CHANGES_REQUESTED, approval keywords, post-SHA-change, no pending trigger) all pass, the agent MUST NOT exit the loop on a single 120-second poll. Step 5 requires the absolute request deadline to remain in force across multiple polls. Exiting after one poll provides only a 120-second response window for the fresh trigger — far short of what the polling schedule is designed to give the reviewer. Require at least one full `120 + 60 + 60 = 240 seconds` cycle before classification, and treat the 120-second-first-poll short-circuit as a hard violation of the skill contract.

**D. Still pending?**
- `classification.decision == "pending"`, AND
- `classification.response_counts.top_level == 0`, AND
- No request-scoped formal `APPROVED` / `CHANGES_REQUESTED` records, AND
- No request-scoped inline comments with concrete requested changes, AND
- All bodies are boilerplate-only
→ **Still waiting** — continue polling

**E. Real feedback detected?**
An entry in `classification.sources.inline_comments`,
`classification.sources.top_level`, or `classification.sources.formal_reviews`
contains concrete code feedback (specific file paths, function names, variable
names, line numbers):
→ **Has blockers or suggestions** — apply fixes, commit, push. Only re-request after fix+push if previous `{{REVIEW_COMMAND}}` already received a response. If no response yet, keep polling.

**F. Ambiguous feedback with unclear actionable intent only?**
- Request-scoped comments exist but none specify a concrete code change
→ **Clarification** — post a reviewer-directed clarification with `{{REVIEW_COMMAND}}` if no request is pending; otherwise keep polling.

**G. Only nits or suggestions?**
- Request-scoped comments are nits or optional improvements, with no
  `CHANGES_REQUESTED`
→ **Suggestions** — fix if straightforward; skip if redesign required. Only re-request after fix+push if previous request received a response.

#### Step 7: Apply fixes

**Hard rule — never exit after pushing a fix.** After `git push` in Step 7, the agent MUST continue to Step 5 to poll for the reviewer's next response.

**Hard rule — never exit with `CHANGES_REQUESTED` unresolved.** If a `CHANGES_REQUESTED` review exists after applying fixes, do not declare the run done. Re-request review (Step 4) and continue the loop. Only approval (formal case A or informal case C), an explicit skill-defined irrecoverable stop condition, or max passes reached may end the loop. Applying a fix that you believe addresses the reviewer's concern does NOT close the loop — the reviewer must explicitly approve.

- Read `.sandman/state/<N>.addressed_comments` — skip any inline comment IDs already present.
- Read relevant source files, make minimal changes.
- Run project tests and formatting (e.g., `go test ./...`, `gofmt -w .`).
- Commit: `git add -A && git commit -m "refactor: address review feedback on #<N>"`
- Push: `git push`
- Append acted-on inline comment IDs to `.sandman/state/<N>.addressed_comments`.
- **After pushing, loop back to Step 2 to wait for the new CI run triggered by the push.** Do not proceed to Step 8 until CI reaches a terminal state.

#### Step 8: Repeat

Go to Step 1 for the next pass. Before re-requesting in Step 4: if head SHA changed → always allow re-request; if SHA unchanged and previous request received no response → keep polling.

### State files

- `.sandman/state/<N>.head_sha` — rewritten on every new review request post. SHA change = all prior review state stale, fresh request always permitted.
- `.sandman/state/<N>.addressed_comments` — cleared when head SHA changes (new commit invalidates old inline comment IDs). One inline comment ID per line.

### Same comment ID 3+ passes without resolution

If an inline comment ID appears in 3+ consecutive passes without resolution, treat it as unresolvable without a larger redesign:
- Do not keep looping on the same comment ID
- Re-request review instead, noting the comment ID

## Never give up conditions

Stop only when:
- Formal approval (A or C) — the **only** condition that completes the PR-Review phase. "Exhausted after 10 passes" or any other non-approval signal is **never** a reason to mark PR-Review complete; only Approval is.
- An explicit skill-defined irrecoverable condition prevents further autonomous progress
- Max 10 passes reached with unresolved blockers AND no new commit has landed on the PR branch since the last `{{REVIEW_COMMAND}}` post (i.e., the prior exhausted budget is still on the latest SHA). This ends the loop with a `REVIEW_TIMEOUT` documented in `.sandman/task.md` and the run log, not a completion — the run-level checklist item stays unchecked until Approval is observed.
- **`REVIEW_CONFLICT_UNRESOLVED` — back-merge failed to resolve a DIRTY PR; not a `REVIEW_TIMEOUT`, never silent**

Continue polling when:
- Review pending / no reviews yet
- Only boilerplate comments exist
- Only nits/suggestions remain
- CI is still running
- **The PR is DIRTY mid-poll — Step 5a triggers `sandman-back-merge`, then restarts polling from Step 1 after a successful push. Keep going while back-merge is making progress; exit with `REVIEW_CONFLICT_UNRESOLVED` only when back-merge itself fails.**
- Any `CHANGES_REQUESTED` review exists but is addressable
- Only already-addressed inline comment IDs remain
- Top-level PR conversation has a new non-trigger comment
- **A new commit has landed (head SHA changed) — re-request always permitted regardless of prior response state, and the 10-pass counter resets to 0 for the new SHA (intra- or inter-session)**
- **A `CHANGES_REQUESTED` review references the issue's acceptance criteria and you have not yet implemented the change OR obtained the reviewer's explicit agreement on the narrowed scope (Hard Rule 7)**

## Tips

- Use the change-request view's `state` and `mergeStateStatus` JSON fields to check merge readiness after approval.
- Always include `top=<count> reviews=<count> inline=<count>` in the final report.
- Never force-push or amend commits.
- Keep commits focused: one commit per review round.
- When feedback is ambiguous, post a reviewer-directed clarification with `{{REVIEW_COMMAND}}` in the same comment.
- Review agents may post feedback as: top-level comments, inline diff comments, or formal `COMMENT` reviews. Always check all three sources.
- When CI is broken and the failure may be base-branch drift, load `sandman-back-merge` first so any fix that landed on the base branch can be merged before retrying.
- When CI is failing, fix it first — CI must be green before any review feedback can be meaningfully addressed.
- **DIRTY PR handling is a hard per-poll guard.** If `mergeable == CONFLICTING` is observed on ANY poll iteration, Step 5a triggers `sandman-back-merge` automatically. Do not treat a DIRTY PR as a manual-resolution situation, do not classify it as `REVIEW_TIMEOUT`, and do not exit the loop with silent success. The only acceptable outcomes are: (a) back-merge succeeded → push → restart polling; (b) back-merge failed → exit with `REVIEW_CONFLICT_UNRESOLVED`.
