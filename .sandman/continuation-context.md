## Completed
Implemented continue branch preservation: `sandman continue` now replays stored base branch for prompt rendering and event metadata, skips base-branch sync on continuation, and keeps the existing worktree unchanged. Updated docs and tests.

## Pending
PR creation, review loop, and merge.

## Blockers
None locally.

## Key Decisions
Replayed `base_branch` comes from the prior run event payload; continuation ignores current config changes. Sync remains enabled for normal runs, disabled for continuation runs.

## Next Step
Push branch and open PR.
