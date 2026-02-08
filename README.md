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
