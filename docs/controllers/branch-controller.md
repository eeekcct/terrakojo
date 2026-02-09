# BranchReconciler Specification

Source: `internal/controller/branch_controller.go`

## Purpose
`BranchReconciler` converts one `Branch` resource into zero or more `Workflow`
resources based on changed files and `WorkflowTemplate` matching.
It also cleans up branch-owned workflows and handles branch lifecycle policies
for default and non-default branches.

## Watched Resources and Triggers
- Primary watch: `Branch` (`For(&Branch{})`).
- Owned watch: `Workflow` (`Owns(&Workflow{})`).
- Reconcile triggers:
- `Branch` create/update/delete events.
- owned `Workflow` events.
- No periodic polling requeue in normal path.
- Deletion finalization uses explicit `RequeueAfter: 5s` while workflows remain.

## Inputs and Dependencies
- Branch spec fields used:
- `spec.owner`
- `spec.repository`
- `spec.name` (Git ref name, for default-branch decision)
- `spec.sha`
- `spec.prNumber` (switches changed-files API)
- Branch annotation used:
- `terrakojo.io/last-sha`
- Workflow parameter key written by this controller:
- `spec.parameters["isDefaultBranch"]` (`"true"` when `branch.spec.name == repo.spec.defaultBranch`, otherwise `"false"`).
- Required dependency:
- `GitHubClientManager` (`GetClientForBranch` must succeed).
- Required related resources:
- owning `Repository` (resolved from `OwnerReferences`, kind `Repository`);
- `WorkflowTemplate` resources in the same namespace.
- Kubernetes index dependency:
- field index `metadata.ownerReferences.uid` for `Workflow` lookup by owning `Branch` UID.

## Reconciliation Flow
1. Fetch `Branch`.
1. If not found: return without error.
1. If deleting:
- finalizer `terrakojo.io/cleanup-workflows` must run;
- delete all owned workflows;
- if any owned workflow still exists, requeue `5s`;
- remove finalizer when no owned workflows remain.
1. If not deleting:
- ensure finalizer exists; if added, return and wait next reconcile.
1. Resolve owning `Repository` from `OwnerReferences`.
- if repository is missing: delete this branch and stop.
1. Compute `isDefaultBranch` by comparing `branch.spec.name` to `repo.spec.defaultBranch`.
1. Read `lastSHA` from `terrakojo.io/last-sha`.
1. List workflows owned by this branch.
1. If `lastSHA == current SHA`:
- no workflows:
  - default branch: delete branch;
  - non-default branch: keep branch.
- all workflows terminal (`Succeeded|Failed|Cancelled`):
  - default branch: delete branch;
  - non-default branch: keep branch.
- otherwise (running/non-terminal): no-op.
1. If SHA changed (`lastSHA` exists and differs): delete existing owned workflows.
1. Create GitHub client for branch.
1. Fetch changed files:
- use PR files API when `spec.prNumber != 0`;
- otherwise use commit files API for `spec.sha`.
1. If changed files are empty:
- default branch: delete branch;
- non-default branch: update `terrakojo.io/last-sha` and keep.
1. List `WorkflowTemplate` resources in namespace.
1. Match templates against changed files (`doublestar` globs).
1. If no template matches:
- default branch: delete branch;
- non-default branch: update `terrakojo.io/last-sha` and keep.
1. For each matched template:
- group matched files by folder (`path.Dir`);
- create one `Workflow` per folder;
- `Workflow.Spec.Path` is set to that folder.
- each created Workflow gets `spec.parameters["isDefaultBranch"]` as a snapshot of the branch classification at creation time.
1. Update branch annotation `terrakojo.io/last-sha = spec.sha`.
1. Update branch status:
- `status.workflows` (created workflow names),
- `status.changedFiles`,
- condition `WorkflowReady=True` / reason `WorkflowCreated`.

## Template Matching and Workflow Fan-out
- Matching uses `WorkflowTemplate.spec.match.paths` against changed files.
- A template may create multiple workflows when multiple folders match.
- Files at repository root (`path.Dir(file) == "."`) are ignored by folder split and do not create workflows.
- Workflow `GenerateName` is `<template-name>-`.

## State and Idempotency
- `terrakojo.io/last-sha` prevents duplicate workflow creation for the same SHA.
- SHA change causes full replacement of branch-owned workflows.
- Terminal completion check requires at least one workflow; empty set is handled separately.
- Default branch and non-default branch have intentionally different retention behavior.

## Failure Handling
- Missing `GitHubClientManager`: hard error.
- Missing repository for a branch: branch is deleted (not error).
- Changed-files API failure: hard error.
- Template list failure: condition `WorkflowCreateFailed=False` is set (best effort), then error is returned.
- Workflow create/update/status failures: hard error.
- During finalization, workflow deletion/list failures: hard error.

## Cleanup and Deletion Semantics
- Finalizer: `terrakojo.io/cleanup-workflows`.
- Branch deletion is blocked until all owned workflows are deleted.
- Default-branch commit branches are ephemeral and removed after no work or terminal completion.
- Non-default branches are retained after completion and are expected to be pruned by `RepositoryReconciler` when ref disappears upstream.

## Observability
Important log events:
- `Waiting for workflows to finish before deleting branch`
- `Repository not found for branch; deleting branch`
- `Deleted existing workflows for branch due to SHA change`
- `No changed files for Branch`
- `No matching WorkflowTemplate found for Branch`
- `Created Workflow for Branch`
- `Deleted Branch after all workflows completed`
- `All workflows completed; keeping Branch`

## Operational Notes
- For non-default branches, "no change" and "no matching template" still advance `terrakojo.io/last-sha`.
- For default branch commit branches, no-op branches are aggressively removed.
- Folder fan-out can produce many workflows for a single branch SHA if many directories changed.
- `spec.parameters["isDefaultBranch"]` is a creation-time snapshot; existing Workflows are not mutated if repository default branch config changes later.

## Test Coverage Map
Primary tests: `internal/controller/branch_controller_test.go`

Covered scenarios:
- finalizer add/remove and deletion wait/requeue behavior.
- keep non-default branch after terminal workflow completion.
- workflow creation from changed files and template matching.
- workflow recreation on SHA change (including after completion).
- PR changed-files API path when `prNumber` is set.
- no-match and root-file behavior.
- helper behavior (`setCondition`, `allWorkflowsCompleted`, owner lookup, list/create helper errors).
- broad error table for:
  - list/update/delete failures,
  - GitHub manager/client failures,
  - changed-files/template/status failures.

Known coverage gaps:
- limited assertions on ordering/determinism of generated workflow list when multiple templates and folders match.
- no dedicated assertion that large template sets maintain acceptable reconcile latency.
