# Terrakojo Controller Overview

## Custom Resources (v1alpha1)
- **Repository**: spec `{owner,name,type,defaultBranch,githubSecretRef{name[,namespace]}}`; status `conditions`, `synced`, `lastDefaultBranchHeadSha` (default-branch sync cursor).
- **Branch**: spec `{owner,repository,name,sha,prNumber?}`; status `conditions`, `workflows[]`, `changedFiles[]`.
- **Workflow**: spec `{owner,repository,branch,sha,template,path?,parameters?}`; status `conditions`, `phase`, `jobs[]` (legacy/compatibility field; not actively maintained), `checkRunID`, `checkRunName`.
  - Controller-managed parameter keys:
    - `spec.parameters["isDefaultBranch"]` (`"true"`/`"false"`).
    - `spec.parameters["executionUnit"]` (`"folder"|"repository"|"file"`).
    - `spec.parameters["workspaceClaimName"]` / `spec.parameters["workspaceMountPath"]`.
- **WorkflowTemplate**: spec `{displayName, match.paths[], match.executionUnit?, workspace?, job}`.

## Controllers
- **RepositoryReconciler** (`internal/controller/repository_controller.go`)
  - Ensures labels (`terrakojo.io/owner`, `terrakojo.io/repo-name`, `terrakojo.io/repo-uid`, managed-by) and finalizer `terrakojo.io/cleanup-branches`.
  - Creates/updates **Branch** CRs from GitHub directly:
    - Non-default: keeps the latest head per ref (with PR number if open), deletes Branches for refs no longer in GitHub.
    - Default: compares `status.lastDefaultBranchHeadSha` to head and creates Branch per commit SHA.
  - Updates `status.lastDefaultBranchHeadSha` after successful default-branch sync.
  - On Repository deletion: deletes owned Branches, waits for completion, then removes finalizer.

- **BranchReconciler** (`internal/controller/branch_controller.go`)
  - Finalizer `terrakojo.io/cleanup-workflows`; on deletion removes owned Workflows first.
  - SHA unchanged (annotation `terrakojo.io/last-sha`) → no-op; SHA change → delete existing Workflows.
  - Fetches changed files for PR via GitHub client; matches WorkflowTemplates; creates Workflows per template and `executionUnit`:
    - `folder` (default): one Workflow per matched folder.
    - `repository`: one Workflow with path `"."`.
    - `file`: one Workflow per matched file path.
  - Propagates default-branch context and execution unit into each created Workflow as `spec.parameters["isDefaultBranch"]` and `spec.parameters["executionUnit"]`.
  - When `WorkflowTemplate.spec.workspace.enabled=true`, creates a target-scoped workspace PVC and passes it via `spec.parameters["workspaceClaimName"]`/`["workspaceMountPath"]` (reused across templates for the same branch/SHA/executionUnit/path target).
  - **Completion cleanup**: when all owned Workflows are terminal (Succeeded/Failed/Cancelled), deletes the Branch only for default-branch commits; non-default branches are kept until GitHub no longer lists them.
  - Uses field index `metadata.ownerReferences.uid` for Workflow lookup.

- **WorkflowReconciler** (`internal/controller/workflow_controller.go`)
  - Creates GitHub CheckRun and a Job from `WorkflowTemplate.spec.job`.
  - Injects reserved runtime env vars into all Job containers/initContainers (`TERRAKOJO_*`, including `TERRAKOJO_IS_DEFAULT_BRANCH`, `TERRAKOJO_EXECUTION_UNIT`, and `TERRAKOJO_WORKSPACE_DIR`).
  - Mounts the shared workspace PVC to all containers when workspace parameters are present.
  - Maps Job status → Workflow `phase` and updates CheckRun.
  - Finalizer cancels CheckRun on deletion if Job not finished; if owning Branch is missing, deletes the Workflow.

## Controller Specs
- Detailed per-controller specifications:
  - `docs/controllers/README.md`
  - `docs/controllers/repository-controller.md`
  - `docs/controllers/branch-controller.md`
  - `docs/controllers/workflow-controller.md`

## Webhook (`internal/webhook/webhook.go`)
- Handles GitHub push/PR events and requests a Repository sync.
- Annotation update with optimistic retry:
  - Sets `terrakojo.io/sync-requested-at` on the matching Repository.
- Uses labels `terrakojo.io/owner`, `terrakojo.io/repo-name` to find the Repository.

## GitHub Auth Helpers (`internal/kubernetes`)
- Secrets: PAT (`token`) or GitHub App (`github-app-id`, `github-installation-id`, `github-private-key`).
- GitHubClientManager finds Repository for a Branch (owner ref or labels) and builds GitHub client.

## Flow Summary
1. Webhook updates Repository annotation to request sync.
2. RepositoryReconciler fetches GitHub and creates/updates Branch CRs.
3. BranchReconciler creates Workflows from templates + changed files.
4. WorkflowReconciler runs Job & CheckRun; BranchReconciler deletes default-branch commit Branches after all Workflows finish.

## Notes / Pitfalls
- Default branch commits are represented by per-commit Branch CRs; non-default branches keep one Branch CR per ref.
- Repository controller prunes non-default Branch CRs that disappear from GitHub; Branch controller deletes default-branch commit Branches after workflows finish.
- Conflict handling: Repository annotation updates use `RetryOnConflict` in webhook.
- Repository status no longer stores branch lists or commit queues; Branch CRs are the source of truth.
- `config/samples/kustomization.yaml` includes user-authored resources (`Repository`, `WorkflowTemplate`) by default.
- `Branch` and `Workflow` sample manifests are reference-only because they are controller-managed in normal operation.

## Useful Entry Points
- CRDs: `api/v1alpha1/*_types.go`
- Controllers: `internal/controller/*.go`
- Webhook: `internal/webhook/webhook.go`
- GitHub wiring: `internal/kubernetes/*.go`
