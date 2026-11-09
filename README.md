# terrakojo

Kubernetes controller that reacts to GitHub push/PR events, creates Branch/Workflow custom resources, runs Jobs via templates, and reports back with CheckRuns.

Quick entry point for this repo:
- Project overview: `docs/overview.md` (summary of CRDs, controllers, webhook, GitHub auth, and flow)

## Install/Deploy Apply Mode
- `make install` and `make deploy` pass `$(KUBECTL_APPLY_FLAGS)` to `kubectl apply`.
- Default is `--server-side=true` to avoid CRD annotation size limits.
- Override as needed, for example:
  - `make install KUBECTL_APPLY_FLAGS=`
  - `make deploy KUBECTL_APPLY_FLAGS=`

## Dependency updates

Renovate is configured in `renovate.json5`. Its standard managers cover Go
modules, Docker images, GitHub Actions (including the golangci-lint input),
and devcontainer dependencies. GitHub Actions are pinned to commit digests. Docker dependencies are pinned to digests so
changes to floating image tags such as distroless `nonroot` remain reviewable.
Custom regex managers also track the pinned
Kustomize, controller-tools, and golangci-lint versions in `Makefile`. The
linter's Makefile and CI versions are grouped into one update. The Go directive
in `go.mod` is explicitly enabled for updates.

Updates require review; automerge is disabled. The Dependency Dashboard shows
pending updates and errors. At most five update PRs are open concurrently and
two new PRs are created per hour. No repository-level time schedule is imposed.
The Renovate app must have access to this repository and run against the default
branch; this configuration does not install or schedule the app itself.

The envtest release and Kubernetes versions are derived from Go dependencies,
so they are not separately pinned. The E2E workflow's floating `latest` kind and
kubectl installs are unchanged by this configuration.
