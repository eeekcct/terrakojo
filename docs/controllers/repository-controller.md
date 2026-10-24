# RepositoryReconciler Specification

Source: `internal/controller/repository_controller.go`

## Purpose
`RepositoryReconciler` keeps `Branch` resources aligned with GitHub for one `Repository`.
It guarantees:
- required labels and finalizer exist on `Repository`;
- default branch commits are represented as per-commit `Branch` resources;
- local non-default refs are represented as one latest-head `Branch` per ref;
- external PR heads are represented independently by PR number in the base repository;
- stale non-default `Branch` resources are deleted;
- owned `Branch` resources are cleaned up before repository deletion finalizes.

## Watched Resources and Triggers
- Primary watch: `Repository` (`For(&Repository{})`).
- No `Owns(&Branch{})` watch is configured.
- Reconcile triggers:
- `Repository` create/update/delete events.
- Annotation changes on `Repository` (for example `terrakojo.io/sync-requested-at`, `terrakojo.io/bootstrap-reset`).
- Explicit requeue every `30m` (`repositorySyncInterval`) after successful reconcile.

## Inputs and Dependencies
- Repository spec fields used:
- `spec.owner`
- `spec.name`
- `spec.defaultBranch`
- `spec.githubSecretRef`
- Repository status fields used:
- `status.lastDefaultBranchHeadSha`
- `status.lastBootstrapResetID` (last atomically applied reset identity)
- Required dependency:
- `GitHubClientManager` (`GetClientForRepository` must succeed).
- Kubernetes index dependency:
- Field index `metadata.ownerReferences.uid` for `Branch` lookup by owning `Repository` UID.

## Reconciliation Flow
1. Fetch `Repository` by name/namespace.
1. If not found: return without error.
1. If deleting:
- ensure branch cleanup via finalizer `terrakojo.io/cleanup-branches`;
- delete all owned `Branch` resources;
- requeue every `5s` while any owned `Branch` still exists;
- remove finalizer when cleanup is complete.
1. If not deleting:
- ensure finalizer exists; if added, return and wait next reconcile.
- ensure labels exist and are correct:
  - `terrakojo.io/owner`
  - `terrakojo.io/repo-name`
  - `terrakojo.io/repo-uid`
  - `app.kubernetes.io/managed-by=terrakojo-controller`
- if labels were updated, return and wait next reconcile.
1. Build GitHub client from repository secret/auth config.
1. List existing `Branch` resources owned by this repository (field index).
1. Sync from GitHub:
- Sync default branch commits:
  - fetch default branch head SHA;
  - for `terrakojo.io/bootstrap-reset=true`, persist a unique controller-owned `terrakojo.io/bootstrap-reset-id` before initializing;
  - if bootstrapping (cursor empty, or the reset ID differs from `status.lastBootstrapResetID`):
    - atomically set `status.lastDefaultBranchHeadSha` to current head SHA and record the reset ID in `status.lastBootstrapResetID` when resetting;
    - set condition `BootstrapReady=True` with reason `InitializedFromHead`;
    - do not create default-branch commit `Branch` resources in this reconcile.
  - otherwise:
    - compute commit SHAs between `status.lastDefaultBranchHeadSha` and head;
    - create one `Branch` per missing SHA (default branch ref);
    - update `status.lastDefaultBranchHeadSha` to the last processed SHA.
- Sync non-default branch heads:
  - fetch branch heads and open PRs;
  - prefer same-repository PR head SHA for a local ref;
  - isolate external/unknown-origin PR heads by PR number;
  - ensure one `Branch` per local ref or external PR;
  - delete stale non-default `Branch` resources not present in GitHub.
1. If `terrakojo.io/bootstrap-reset=true` was consumed successfully, controller atomically writes `done` and removes `terrakojo.io/bootstrap-reset-id`. A later explicit `true` therefore receives a new identity, including at the same HEAD.
1. Return `RequeueAfter: 30m`.

## Default Branch Commit Sync Details
- First sync (`lastDefaultBranchHeadSha` empty): current head SHA is recorded as cutover cursor, and no default-branch commit Branch is created.
- If cursor equals current head: no default-branch creation.
- For historical catch-up: GitHub compare API is used via `CompareCommits`.
- Compare failure fallback:
- fallback to current head SHA only when previous cursor commit no longer exists (history rewrite / force-push);
- otherwise return error to avoid skipping commits.
- Batch behavior:
- compare API page size is 250 commits;
- when backlog is larger, one reconcile processes a subset and stores the last processed SHA as new cursor;
- next reconcile continues from that cursor.

## Non-Default Branch Sync Details
- Local refs retain one desired head per ref. When several same-repository PRs
  use that ref, the highest PR number wins. Its SHA takes precedence over the
  branch-list SHA, with the latter used only when the local PR SHA is missing.
- Fork PRs and PRs with unknown/deleted head repositories use independent PR
  identities, scoped to the base repository. Two PRs from the same fork/ref are
  also independent. Their SHA must come from the PR head, never a local ref.
- Head/base repository IDs identify same-repository PRs when both are available;
  otherwise explicit head repository owner/name is compared case-insensitively
  with the configured repository. A missing head repository is not assumed local.
- A head with no SHA is skipped. Its known external PR identity is still retained
  for legacy-resource migration, preventing accidental reuse as a local branch.
- Same identity and SHA: retain the existing object. A changed SHA (or changed
  literal ref on an external PR) replaces only that identity's Branch.
- Local resource names are unchanged:
  `<repository-name>-<hash(ref)[:8]>-<short-sha[:8]>`.
- External PR resource names are:
  `<repository-name>-pr-<pr-number>-<short-sha[:8]>`.
  The controller-managed annotation `terrakojo.io/pull-request-identity` records
  the PR number. Do not edit this identity annotation manually.
- `spec.name` remains the literal Git ref; `spec.owner` and `spec.repository`
  remain the base repository. Workflow matching, runtime ref/PR values, changed-file
  queries, CheckRuns and credential selection retain those meanings. This does
  not add a checkout implementation or change which repository receives credentials.
- The existing default-ref exclusion remains: a PR whose head ref equals
  `spec.defaultBranch`, including a fork's `main`, is not materialized as a
  non-default Branch. Default commit processing is unchanged.

### Upgrade behavior
Existing unannotated Branches matching a currently open external PR number and
head ref are deleted and recreated once with the new identity. Their dependent
Workflows may be cancelled and recreated. The controller waits for existing
finalizers to finish before reusing a name for a local branch. Normal local
branches retain their existing names and objects.

Closed or absent historical PRs cannot be classified from the open-PR snapshot;
their unannotated objects retain the previous cleanup behavior. If upgrading a
development installation that contains such ambiguous historical resources,
reset those controller-managed Branches and their dependent Workflows before
relying on the new identity scheme. No live, interruption-free migration or
rollback compatibility is promised; do not downgrade with new external-identity
resources still present.

## State and Idempotency
- Label enforcement is idempotent.
- Finalizer handling is idempotent.
- Default branch creation is deduplicated by existing SHA.
- Non-default heads converge to one latest object per local ref or external PR.
- Re-running reconcile with unchanged GitHub state yields no net resource changes or redundant condition/status writes.
- A successful atomic cursor/status write defines the cutover. If it fails, the cursor and applied reset ID remain unchanged; a retry may initialize from the then-current HEAD.
- Once an ID is recorded in status, retries never reinitialize that reset. They continue normal commit processing from the persisted cursor, even after branch/PR sync or reset acknowledgment failure.
- A partially advanced cursor is never rewound to the original cutover: completed and deleted commit Branches must not be replayed.
- Metadata updates retain resource-version conflict checks. A conflicting acknowledgment is returned for a fresh reconcile, not blindly retried against a potentially cancelled or replaced request.
- An observed cancellation (`bootstrap-reset` removed or not `true`) removes a stale managed request ID. Wait for `done`, or for cancellation to be observed, before issuing another reset; writing `true` again while the same request remains pending is a retry, not a new request.
- `LastTransitionTime` changes only when condition status changes, not for repeated failures or message changes.

## Failure Handling
- Missing `GitHubClientManager`: hard error.
- GitHub auth/client creation failure: hard error.
- Branch list/create/update/delete failures: hard error.
- Status update failure (`lastDefaultBranchHeadSha`): hard error.
- Cutover lookup or persistence failure marks `status.conditions[type=BootstrapReady]=False` with reason `InitializationFailed`. Downstream branch/PR synchronization and reset acknowledgment errors do not invalidate an already-persisted cutover or change this condition.
- Compare behavior prevents silent commit skips except explicit history-rewrite fallback.

## Cleanup and Deletion Semantics
- Finalizer: `terrakojo.io/cleanup-branches`.
- On repository deletion:
- all owned `Branch` resources are deleted first;
- reconciler waits until no owned branches remain;
- finalizer is removed only after cleanup is complete.

## Observability
Important log events:
- `Updating Repository labels`
- `Successfully created GitHub client for repository`
- `Created Branch for default branch commit`
- `Updated Branch resource metadata`
- `Deleted Branch resource with old SHA`
- `Deleted Branch resource no longer desired`
- `Waiting for branches to be removed before finalizing repository`
- `Cleaned up branches before repository deletion`

## Operational Notes
- Poll fallback interval is intentionally long (`30m`) and webhook-triggered sync is expected to handle near-real-time updates.
- Default branch processing is incremental for large backlogs.
- Bootstrap reset: set `metadata.annotations["terrakojo.io/bootstrap-reset"]="true"` to reinitialize cutover from current default-branch head. Controller marks completion with `"done"`.
- `terrakojo.io/bootstrap-reset-id` and `status.lastBootstrapResetID` are controller-managed bookkeeping, not user configuration. Do not edit or copy them into Repository manifests.
- Install the updated Repository CRD before running this controller version so the API server preserves `status.lastBootstrapResetID`.
- This controller does not react directly to `Branch` changes; it reacts to `Repository` events plus periodic polling.

## Test Coverage Map
Primary tests: `internal/controller/repository_controller_test.go`, `repository_bootstrap_retry_test.go`, and `repository_bootstrap_state_test.go`

Covered scenarios:
- bootstrap retries across branch/PR fetch, request-ID, cutover-status, and acknowledgment write failures;
- advanced cursor preservation, acknowledgment conflicts/cancellation, fresh repeated resets, and no-op condition/status writes;
- finalizer and required labels are added.
- default branch commit creates `Branch` resource.
- non-default branch + PR metadata creates `Branch` with `prNumber`.
- local and two fork PRs with identical ref/SHA remain independent (fake client and envtest).
- advancing or closing one fork leaves the other fork/local objects unchanged.
- missing fork SHA never borrows a local SHA; unknown head repositories are isolated.
- legacy fork recreation waits for finalizers before reusing a local name.
- fork heads named as the default branch remain excluded.
- stale non-default `Branch` resources are pruned.
- error paths:
  - missing GitHub manager;
  - GitHub client creation failure;
  - branch list failure;
  - branch create/update/delete failures;
  - label update failure.

Known coverage gaps:
- no direct envtest assertion for repository deletion finalizer flow waiting for owned `Branch` removal.
- limited direct assertions around incremental cursor advancement across multiple >250 commit batches.
