# deploy/

These manifests are the **desired state** for warden. They are staged here for
review, but the source of truth is `jedarden/declarative-config`. Per the
cluster rule, nothing here is applied with `kubectl` directly — copy into
`declarative-config` at `k8s/rs-manager/warden/`, commit, and let ArgoCD sync.

## Contents

- `deployment.yaml` — warden Deployment (non-root, read-only rootfs, pinned image digest).
- `service.yaml` — ClusterIP.
- `ingressroute.yaml` — Traefik IngressRoute on the cluster's single Tailscale ingress (tailnet-only).
- `sealedsecret.md` — how to build the `warden-secrets` SealedSecret (refresh token, org namespace, caller tokens).
- `warden-build.workflowtemplate.yaml` — Argo WorkflowTemplate; also belongs in `declarative-config` at `k8s/iad-ci/argo-workflows/`.

## Bring-up order

1. Create the dedicated Spot org + a `gp.vs1.medium-iad` node pool (console/Terraform).
2. Verify SpotNodePool field names against the live object (`internal/spot/types.go`).
3. Build the image via the `warden-build` WorkflowTemplate (tags `ronaldraygun/warden` with the repo's `VERSION` file — semver, per declarative-config's rule against `:latest`/bare-digest tags in manifests).
4. Create `warden-secrets` SealedSecret in rs-manager (`sealedsecret.md`).
5. Commit manifests to `declarative-config`; ArgoCD syncs.
6. Point the autoscaler/dispatcher at `https://warden-rs-manager.<tailnet>.ts.net`.

> The image tag must be a pinned digest — never `:latest`.
