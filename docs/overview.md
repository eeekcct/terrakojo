# Terrakojo Controller Overview

## Custom Resources (v1alpha1)
- **Repository**: spec `{owner,name,type,defaultBranch,githubSecretRef{name[,namespace]}}`; status `branchList` (non-default branches), `defaultBranchCommits` (default-branch commit queue), `conditions`, `synced`.
- **Branch**: spec `{owner,repository,name,sha,prNumber?}`; status `conditions`, `workflows[]`, `changedFiles[]`.
- **Workflow**: spec `{owner,repository,branch,sha,template,path?,parameters?}`; status `conditions`, `phase`, `jobs[]`, `checkRunID`, `checkRunName`.
- **WorkflowTemplate**: spec `{displayName, match.paths[], steps[] {name,image,command[]}}`.

## Controllers
- **RepositoryReconciler** (`internal/controller/repository_controller.go`)
  - Ensures labels (`terrakojo.io/owner`, `terrakojo.io/repo-name`, `terrakojo.io/repo-uid`, managed-by) and finalizer `terrakojo.io/cleanup-branches`.
  - Creates/updates **Branch** CRs for:
    - `status.branchList` (non-default only): keeps newest SHA per ref, deletes stale ones.
    - `status.defaultBranchCommits` (default branch queue): creates Branch per commit SHA, never deletes others for same ref.
  - On Repository deletion: deletes owned Branches, waits for completion, then removes finalizer.

- **BranchReconciler** (`internal/controller/branch_controller.go`)
  - Finalizer `terrakojo.io/cleanup-workflows`; on deletion removes owned Workflows first.
  - SHA unchanged (annotation `terrakojo.io/last-sha`) → no-op; SHA change → delete existing Workflows.
  - Fetches changed files for PR via GitHub client; matches WorkflowTemplates; creates Workflows per template & folder.
  - **Completion cleanup**: when all owned Workflows are terminal (Succeeded/Failed/Cancelled), removes this commit from:
    - `Repository.status.defaultBranchCommits` if default branch, else
    - `Repository.status.branchList` (ref+SHA match),
    then deletes the Branch CR.
  - Uses field index `metadata.ownerReferences.uid` for Workflow lookup.

- **WorkflowReconciler** (`internal/controller/workflow_controller.go`)
  - Creates GitHub CheckRun and a Job from the first step of the WorkflowTemplate.
  - Maps Job status → Workflow `phase` and updates CheckRun.
  - Finalizer cancels CheckRun on deletion if Job not finished; if owning Branch is missing, deletes the Workflow.

## Webhook (`internal/webhook/webhook.go`)
- Handles GitHub push/PR events, converts to `BranchInfo`.
- Status update with optimistic retry:
  - Default branch ⇒ append SHA to `defaultBranchCommits` if absent.
  - Non-default ⇒ ensure ref+SHA unique in `branchList` (delete duplicate, then append).
- Uses labels `terrakojo.io/owner`, `terrakojo.io/repo-name` to find the Repository.

## GitHub Auth Helpers (`internal/kubernetes`)
- Secrets: PAT (`token`) or GitHub App (`github-app-id`, `github-installation-id`, `github-private-key`).
- GitHubClientManager finds Repository for a Branch (owner ref or labels) and builds GitHub client.

## Flow Summary
1. Webhook writes commit info into Repository status (queue or list).
2. RepositoryReconciler creates/updates Branch CRs.
3. BranchReconciler creates Workflows from templates + changed files.
4. WorkflowReconciler runs Job & CheckRun; BranchReconciler cleans status and deletes Branch after all Workflows finish.

## Notes / Pitfalls
- Default branch commits live only in `defaultBranchCommits`; non-default branches live only in `branchList`.
- Repository controller skips deleting default-branch Branches; Branch controller handles cleanup after workflows finish.
- Conflict handling: Repository status updates use `RetryOnConflict` in both webhook and Branch controller.
- Sample manifests in `config/samples` for Branch/Workflow are outdated (missing required fields).

## Useful Entry Points
- CRDs: `api/v1alpha1/*_types.go`
- Controllers: `internal/controller/*.go`
- Webhook: `internal/webhook/webhook.go`
- GitHub wiring: `internal/kubernetes/*.go`
