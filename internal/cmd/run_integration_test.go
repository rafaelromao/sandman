// Package cmd documents the decision for the #1326 flaky-in-CI skip
// cluster (#1784 / parent #1778).
//
// Decision (#1778): the #1326 flaky-in-CI skip cluster was
// ported to the unit suite rather than re-enabled. The skipped tests in
// this file all drove the real CLI orchestrator through
// `executeRunCommand` against real `git` worktrees and (for podman
// variants) real containers; their flakiness was rooted in real-process
// timing races that poll-with-deadline helpers cannot fix. Architectural
// refactors that would have made them fixable were out of scope.
//
// The remaining file is the package declaration plus this note. Helpers
// used by both the (deleted) integration tests and the surviving
// unit-style tests in this package live in run_helpers_test.go.
//
// Coverage map — every previously-skipped test, where the load-bearing
// assertions now live:
//
//   - TestRun_ExplicitZeroParallelRunsThroughOrchestratorEndToEnd
//     → TestRunBatch_ZeroParallelAllowsAllRunsToStart (internal/batch)
//
//   - TestRun_DependencyAwareBatch_InvalidGraphsFailBeforeExecution
//     → cycle cases in internal/batch/dependencies_test.go
//     → TestRunBatch_SkipsIssuesBlockedByOpenExternalBlockers (internal/batch)
//
//   - TestRun_DependencyAwareBatch_BlocksDependentsAfterFailure
//     → TestRunBatch_SkipsDependentsWhenBlockerFails (internal/batch)
//
//   - TestRun_DependencyAwareBatch_NoDependenciesRemainConcurrent
//     → TestRunBatch_PreservesParallelismWithinDependencyLevel (internal/batch)
//
//   - TestRun_WorktreeSandboxSingleIssuePersistsLogAndRemovesWorktree
//     → TestRunBatch_PreservesWorktreeOnSuccess (internal/batch)
//
//   - TestRun_WorktreeSandboxOverrideFlagClearsArtifacts
//     → TestRunBatch_OverrideClearsExistingBranchesAndProceeds (internal/batch)
//
//   - TestRun_DefaultSandboxSingleIssue_MissingDockerfileFailsBeforeAgentRunBegins
//     → TestRunBatch_ContainerModeFailsBeforeAgentWhenDockerfileMissing (internal/batch)
//
//   - TestRun_DefaultSandboxSingleIssueUsesContainerWorkdirAndCleansUpWorktree
//     → container workdir binding in internal/sandbox/container_sandbox_test.go
//     → TestRunBatch_PreservesWorktreeOnSuccess (internal/batch)
//
//   - TestRun_DefaultSandboxTwoIssuesReuseContainerAndSeparateWorktrees
//     → TestRunBatch_ContainerCapacityOneStartsOneContainerPerConcurrentRun (internal/batch)
//
//   - TestRun_DefaultSandboxTwoIssuesQueueWithSingleContainerSlot
//     → TestRunBatch_MaxContainersLimitRestrictsSharedContainerConcurrency (internal/batch)
//
//   - TestRun_DefaultSandboxFourIssuesAutoModeSpawnsContainersForCapacityAndKeepsWorktreesSeparate
//     → TestRunBatch_MaxContainersAutoSpawnsContainersForCapacity (internal/batch)
//
//   - TestRun_WorktreeSandboxSingleIssuePropagatesAgentEnvToLog
//     → TestAgentRun_Run_PassesEnvAndPromptFileThroughFullChain (internal/batch)
//
//   - TestRun_WorktreeSandboxSingleIssuePreservesWorktreeOnFailure
//     → TestRunBatch_PreservesWorktreeOnInterrupt (internal/batch)
//
//   - TestRun_WorktreeSandboxSingleIssuePreservesRenderedCliPrompt
//     → TestAgentRun_Run_PassesEnvAndPromptFileThroughFullChain (internal/batch)
//
//   - TestRun_PodmanSandboxUsesDotGitconfigIdentityWithoutMutatingWorktreeConfig
//     → TestRunBatch_UsesDotGitconfigIdentityOverRepoLocalConfig (internal/batch)
//
//   - TestRun_PodmanSandboxUsesXDGGitIdentityWithoutMutatingWorktreeConfig
//     → TestRunBatch_UsesXDGGitIdentityWhenDotGitconfigLacksIdentity (internal/batch)
//
//   - TestRun_WorktreeSandboxUsesHostGitIdentityWithoutMutatingWorktreeConfig
//     → TestRunBatch_UsesDotGitconfigIdentityOverRepoLocalConfig (internal/batch)
//
//   - TestRun_PodmanSandboxUsesRepoDefaultIdentityWhenConfigEmpty
//     → TestRunBatch_FallsBackToRepoLocalGitIdentity (internal/batch)
//
//   - TestRun_DependencyAwareBatch_MixedRunnableAndBlockedIssues
//     → TestRunBatch_SkipsIssuesBlockedByOpenExternalBlockers (internal/batch)
//
//   - TestRun_FreshRunErrorsWhenBranchAlreadyExists
//     → TestRunBatch_AbortsUpfrontWhenAnyBranchExists (internal/batch)
//
//   - TestRun_IssueDrivenBatchUsesNewIDScheme
//     → TestRunBatch_PerRowRunIDsShareBatchPrefix (internal/batch)
//     → TestRun_SingleIssueRegistersPerRowRunIDInBatchesIndex (internal/cmd)
//
//   - TestRun_ContinueMode_RunDirAndSocketsBeforeContinuedEvent
//     → TestRun_BootArtifactsBeforeRunStarted (internal/cmd)
//     → TestRun_CreatesControlSocketInRunDirWithCommander (internal/cmd)
//
//   - TestRun_RemovesCommandSocketOnCompletion
//     → TestRun_BootArtifactsBeforeRunStarted (internal/cmd) + lifecycle tests
//
//   - TestRun_SetsRunDirOnBatchRequest
//     → TestRun_SingleIssueRegistersPerRowRunIDInBatchesIndex (internal/cmd)
//
//   - TestRun_DependencyAwareBatch_IncludeDependenciesExecutesTransitiveChain
//     (bespoke in-batch-blocker skip)
//     → blocker-fix tests in internal/batch/orchestrator_test.go
//
//   - TestRun_DependencyAwareBatch_TwoLevelDAGPreservesParallelismWithinLevels
//     (bespoke in-batch-blocker skip)
//     → TestRunBatch_PreservesParallelismWithinDependencyLevel (internal/batch)
package cmd
