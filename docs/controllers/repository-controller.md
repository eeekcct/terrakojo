# RepositoryReconciler Specification

Source: `internal/controller/repository_controller.go`

## Purpose
`RepositoryReconciler` keeps `Branch` resources aligned with GitHub for one `Repository`.
It guarantees:
- required labels and finalizer exist on `Repository`;
- default branch commits are represented as per-commit `Branch` resources;
- non-default refs are represented as one latest-head `Branch` per ref;
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
  - if bootstrapping (`status.lastDefaultBranchHeadSha` is empty, or `terrakojo.io/bootstrap-reset=true`):
    - set `status.lastDefaultBranchHeadSha` to current head SHA;
    - set condition `BootstrapReady=True` with reason `InitializedFromHead`;
    - do not create default-branch commit `Branch` resources in this reconcile.
  - otherwise:
    - compute commit SHAs between `status.lastDefaultBranchHeadSha` and head;
    - create one `Branch` per missing SHA (default branch ref);
    - update `status.lastDefaultBranchHeadSha` to the last processed SHA.
- Sync non-default branch heads:
  - fetch branch heads and open PRs;
  - prefer PR head SHA when PR exists for a ref;
  - ensure exactly one `Branch` resource per non-default ref;
  - delete stale non-default `Branch` resources not present in GitHub.
1. If `terrakojo.io/bootstrap-reset=true` was consumed successfully, controller writes it back to `terrakojo.io/bootstrap-reset=done`.
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
- One desired entry per ref (`BranchInfo`), optionally with PR number.
- Existing branch with same ref and same SHA:
- only metadata update is performed when PR number changed.
- Existing branch with same ref and different SHA:
- old branch is deleted;
- new branch is created with new SHA.
- Resource naming for `Branch`:
- `<repository-name>-<hash(ref)[:8]>-<short-sha[:8]>`.
- ref hash is included so different refs pointing to same SHA do not collide.

## State and Idempotency
- Label enforcement is idempotent.
- Finalizer handling is idempotent.
- Default branch creation is deduplicated by existing SHA.
- Non-default refs converge to one latest branch object per ref.
- Re-running reconcile with unchanged GitHub state yields no net resource changes.

## Failure Handling
- Missing `GitHubClientManager`: hard error.
- GitHub auth/client creation failure: hard error.
- Branch list/create/update/delete failures: hard error.
- Status update failure (`lastDefaultBranchHeadSha`): hard error.
- Bootstrap failure marks `status.conditions[type=BootstrapReady]=False` with reason `InitializationFailed`.
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
- This controller does not react directly to `Branch` changes; it reacts to `Repository` events plus periodic polling.

## Test Coverage Map
Primary tests: `internal/controller/repository_controller_test.go`

Covered scenarios:
- finalizer and required labels are added.
- default branch commit creates `Branch` resource.
- non-default branch + PR metadata creates `Branch` with `prNumber`.
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
