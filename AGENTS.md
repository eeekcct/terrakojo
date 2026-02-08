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

## Notes / pitfalls
- Webhook receiver is deployed as a separate Deployment (plain HTTP server), not as an "event" CRD.
- Commit events must not be missed; handle duplicates/out-of-order and keep a recovery/HA plan beyond GitHub retries.
- Default branch commits are represented as per-commit Branch CRs; non-default branches keep one Branch CR per ref from GitHub sync (no Repository status lists).
- Branch cleanup: default-branch commit Branches are deleted after workflows reach terminal state; non-default Branches are deleted when GitHub no longer lists the ref.
- config/samples default kustomization includes only user-authored resources
  (`Repository`, `WorkflowTemplate`). `Branch` and `Workflow` samples are
  reference-only because they are controller-managed.
