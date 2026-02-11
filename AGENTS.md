# AGENTS

This repository is a Kubernetes controller (kubebuilder). Primary doc: docs/overview.md.

## Tech
- Go 1.24.5 (module github.com/eeekcct/terrakojo)
- controller-runtime, CRDs, webhook

## Key paths
- api/v1alpha1: CRD types
- internal/controller: reconcilers
- internal/webhook: GitHub webhook handling
- internal/kubernetes: GitHub auth/client
- cmd/main.go: manager entry
- config/: CRDs, RBAC, manifests
- docs/overview.md: system behavior notes

## Build / test
- make build: generate + fmt + vet + build
- make test: envtest + go test (non-e2e)
- make lint: golangci-lint
- make run: run controller locally
- make test-e2e: kind-based e2e (requires kind)

## Codegen / manifests
- If you change api types or markers, run:
  - make generate
  - make manifests
- Generated CRDs land in config/crd/bases.

## PR creation rules
- Scope rule (semi-strict): keep `1 PR = 1 feature`.
- Allowed in the same PR: tests/docs/codegen changes that are directly required by that feature.
- Not allowed in the same PR: unrelated feature work or refactors.
- If mixed scope is found, split into feature-specific branches/PRs.

- If a PR changes behavior or configuration, update related docs in the same PR.
- If a PR changes user-facing CRD/spec behavior, update related samples in the same PR.
- If sample updates are intentionally not needed, write the reason in the PR `Non-goals` section.

- Public PRs must not contain private/internal notes (for example, Notion-only context).
- PR description must include:
  - `Summary`
  - `Non-goals`
  - `Validation`

- Minimum validation before opening/updating a PR:
  - make lint
  - env GOCACHE=/tmp/go-build go test ./internal/controller -count=1
- When API types or markers change, also run:
  - make generate
  - make manifests

## Notes / pitfalls
- Webhook receiver is deployed as a separate Deployment (plain HTTP server), not as an "event" CRD.
- Commit events must not be missed; handle duplicates/out-of-order and keep a recovery/HA plan beyond GitHub retries.
- Default branch commits are represented as per-commit Branch CRs; non-default branches keep one Branch CR per ref from GitHub sync (no Repository status lists).
- Branch cleanup: default-branch commit Branches are deleted after workflows reach terminal state; non-default Branches are deleted when GitHub no longer lists the ref.
- config/samples default kustomization includes only user-authored resources
  (`Repository`, `WorkflowTemplate`). `Branch` and `Workflow` samples are
  reference-only because they are controller-managed.
