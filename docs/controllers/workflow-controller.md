# WorkflowReconciler Specification

Source: `internal/controller/workflow_controller.go`

## Purpose
`WorkflowReconciler` executes one `Workflow` by coordinating:
- GitHub Check Runs (`CreateCheckRun` / `UpdateCheckRun`);
- a Kubernetes `Job` created from `WorkflowTemplate`.

It keeps `Workflow.status.phase` aligned with Job state and updates the
corresponding GitHub Check Run status/conclusion.

## Watched Resources and Triggers
- Primary watch: `Workflow` (`For(&Workflow{})`).
- Owned watch: `Job` (`Owns(&Job{})`).
- Reconcile triggers:
- `Workflow` create/update/delete events.
- owned `Job` events.
- No fixed periodic requeue in normal path.

## Inputs and Dependencies
- Workflow spec fields used:
- `spec.owner`
- `spec.repository`
- `spec.branch` (name of `Branch` resource, not Git ref string)
- `spec.sha`
- `spec.template`
- `spec.path`
- `spec.parameters["isDefaultBranch"]` (optional string; passed through to env as-is; empty string if absent; typically `"true"`/`"false"`)
- `spec.parameters["executionUnit"]` (optional string; passed through to env as-is; empty string if absent; commonly `"folder"`/`"repository"`/`"file"`)
- `spec.parameters["workspaceClaimName"]` / `spec.parameters["workspaceMountPath"]` (optional workspace wiring)
- Workflow status fields used:
- `status.checkRunID`
- `status.checkRunName`
- `status.phase`
- Required dependency:
- `GitHubClientManager` (`GetClientForBranch` must succeed when branch exists).
- Required related resources:
- owning `Branch` referenced by `spec.branch`;
- `WorkflowTemplate` referenced by `spec.template`.
- Job convention:
- Job name equals workflow name.

## Reconciliation Flow
1. Fetch `Workflow`.
1. If not found: return without error.
1. Fetch `Branch` (`spec.branch` in same namespace):
- branch get error (non-notfound): return error;
- branch not found: mark `branchNotFound=true`.
1. Ensure `GitHubClientManager` exists.
1. If branch exists, build GitHub client for that branch.
1. Deletion path first:
- if workflow has no finalizer `terrakojo.io/cleanup-checkrun`, return;
- when branch exists, run deletion handler:
  - if `checkRunID == 0`, no-op;
  - if job is complete or missing, no-op;
  - else set Check Run to `completed/cancelled`;
- remove finalizer and update workflow.
1. Non-deletion path:
- ensure finalizer exists; if added, return and wait next reconcile.
- if branch is missing: delete workflow and stop.
1. Try to fetch Job with same name as workflow.
1. If Job exists:
- determine phase from Job status:
  - `Succeeded > 0` -> `Succeeded`
  - `Failed > 0` -> `Failed`
  - `Active > 0` -> `Running`
  - otherwise -> `Pending`
- map phase to Check Run status/conclusion and call `UpdateCheckRun`;
- if phase changed, update workflow status with retry-on-conflict.
1. If Job does not exist:
- fetch `WorkflowTemplate`; not found is ignored.
- build check run name: `<template.displayName>(<workflow.spec.path>)`.
- create GitHub Check Run (queued).
- persist `status.checkRunID` and `status.checkRunName` using conflict-retry update.
- create Job from `template.spec.job`.
- set workflow as controller owner of the Job.
- create Job.
- compute phase for new Job object and update Check Run/status accordingly.

## Job Construction Rules
- `template.spec.job` is copied into the created Job spec.
- Job metadata is controller-assigned:
  - `metadata.name = workflow.name`
  - `metadata.namespace = workflow.namespace`
- If fields are omitted in `spec.job`, the controller applies secure defaults:
  - `backoffLimit = 0`
  - `restartPolicy = Never`
- Job pod hardening defaults:
  - `runAsNonRoot=true` (if unset)
  - `runAsUser=1000` (if unset)
  - `seccompProfile=RuntimeDefault` (if unset, or if type is empty)
  - `containers[]` and `initContainers[]`: `allowPrivilegeEscalation=false` (if unset)
  - `containers[]` and `initContainers[]`: `capabilities.drop=["ALL"]` (if unset or empty)
- Container names are passed through from `template.spec.job`; Kubernetes Job validation rejects invalid names.
- Reserved runtime env vars are injected into all containers and initContainers (overriding same-name template env entries):
  - `TERRAKOJO_OWNER`
  - `TERRAKOJO_REPOSITORY`
  - `TERRAKOJO_WORKFLOW_NAME`
  - `TERRAKOJO_WORKFLOW_NAMESPACE`
  - `TERRAKOJO_WORKFLOW_TEMPLATE`
  - `TERRAKOJO_WORKFLOW_PATH`
  - `TERRAKOJO_SHA`
  - `TERRAKOJO_BRANCH_RESOURCE`
  - `TERRAKOJO_REF_NAME`
  - `TERRAKOJO_PR_NUMBER`
  - `TERRAKOJO_EXECUTION_UNIT`
  - `TERRAKOJO_IS_DEFAULT_BRANCH`
  - `TERRAKOJO_WORKSPACE_DIR`
- `TERRAKOJO_EXECUTION_UNIT` is derived from `spec.parameters["executionUnit"]`:
  - the value is passed through as-is.
  - if the parameter is missing, the env var is injected as an empty string.
- `TERRAKOJO_IS_DEFAULT_BRANCH` is derived from `spec.parameters["isDefaultBranch"]`:
  - the value is passed through as-is.
  - if the parameter is missing, the env var is injected as an empty string.
- `TERRAKOJO_WORKSPACE_DIR` is derived from `spec.parameters["workspaceMountPath"]`:
  - the value is passed through as-is.
  - if the parameter is missing, the env var is injected as an empty string.
- When `spec.parameters["workspaceClaimName"]` is present, the Job gets a PVC volume named `terrakojo-workspace` and mounts it into all containers/initContainers at `workspaceMountPath` (or `/workspace` if mount path is absent).

## Check Run Mapping
- `Pending` -> `queued` (no conclusion)
- `Running` -> `in_progress` (no conclusion)
- `Succeeded` -> `completed` + `success`
- `Failed` -> `completed` + `failure`
- `Cancelled` -> `completed` + `cancelled`

## State and Idempotency
- Finalizer ensures best-effort check-run cancellation before workflow deletion.
- Deterministic Job naming (workflow name) prevents duplicate concurrent jobs for one workflow.
- Status writes use retry-on-conflict to tolerate concurrent updates.
- If workflow is deleted between check-run creation and status write, not-found is ignored to avoid reconcile loops.
- When `status.checkRunID` is already set, reconcile reuses that CheckRun instead of creating a new one.
- When `status.checkRunID` is set but `status.checkRunName` is empty, reconcile rebuilds the default name and persists it before CheckRun updates.
- If Job create returns `AlreadyExists`, reconcile treats it as a race, re-fetches the Job, and continues.
- For `AlreadyExists` races, the re-fetched Job must be controlled by the same Workflow UID; otherwise reconcile fails with ownership mismatch.

## Failure Handling
- Missing `GitHubClientManager`: hard error.
- Branch lookup failure (non-notfound): hard error.
- Missing branch in normal path: workflow is deleted (best effort).
- Check Run create/update failures: hard error.
- Job create/get failures: hard error (except Job creation errors classified as Kubernetes `Invalid` or `Forbidden`, which are treated as terminal success).
- If Job creation fails after Check Run creation, controller best-effort updates Check Run to `completed/failure` and sets workflow phase to `Failed`.
- Job creation errors classified as Kubernetes `Invalid` or `Forbidden` are treated as terminal and return success (`nil`) to avoid retry loops and duplicate CheckRuns.
- Status update failures: hard error unless explicitly treated as not-found race.
- Template not found: treated as ignore-notfound (no error return).

## Cleanup and Deletion Semantics
- Finalizer: `terrakojo.io/cleanup-checkrun`.
- Deleting workflow with active job:
- attempts to mark Check Run cancelled before finalizer removal (branch must still be resolvable).
- If branch is already missing during deletion:
- cancellation path is skipped because GitHub client cannot be constructed;
- finalizer is still removed to unblock deletion.

## Observability
Important log events:
- `Branch not found for Workflow, skipping processing`
- `Failed to create GitHub CheckRun for workflow`
- `Created Job for Workflow`
- `Workflow was deleted during reconcile, ignoring`
- `Cancelled GitHub CheckRun before workflow deletion`

## Operational Notes
- Check Run identity (`status.checkRunID`) is the authoritative link for later updates.
- Workflow status conflict handling is implemented with `RetryOnConflict`.
- The controller uses `status.phase` for lifecycle control.
- `status.jobs` is a legacy compatibility field; the controller neither uses it for control decisions nor actively maintains it.
- Default-branch context for job scripts should be read from `TERRAKOJO_IS_DEFAULT_BRANCH` instead of hard-coding branch names.
- Workflow unit context for job scripts should be read from `TERRAKOJO_EXECUTION_UNIT`.
- Shared workspace usage should read/write through `TERRAKOJO_WORKSPACE_DIR`.

## Test Coverage Map
Primary tests: `internal/controller/workflow_controller_test.go`

Covered scenarios:
- status updates and conflict retry behavior.
- finalizer addition and deletion behavior.
- missing workflow/template/branch paths.
- job creation path and existing-job update path.
- check-run cancellation path on deletion.
- not-found race after check-run creation.
- helper methods (`setCondition`, status retry errors, deletion helper behavior).
- broad error table for:
  - branch/job get failures,
  - missing GitHub manager/client failures,
  - check-run create/update failures,
  - status update failures,
  - job create and owner-reference errors.

Known coverage gaps:
- no end-to-end assertion for user-specified `spec.job` security overrides.
- no explicit scenario validating repeated steady-state reconciles with unchanged running job status.
