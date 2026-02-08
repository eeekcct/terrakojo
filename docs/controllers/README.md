# Controller Specifications

This directory contains operational specifications for Terrakojo reconcilers.
Each document describes reconcile triggers, lifecycle, idempotency model,
failure behavior, cleanup rules, and current test coverage.

## Documents
- [RepositoryReconciler](repository-controller.md)
- [BranchReconciler](branch-controller.md)
- [WorkflowReconciler](workflow-controller.md)

## Scope
- Includes only controllers under `internal/controller`.
- Excludes webhook and other internal packages.

