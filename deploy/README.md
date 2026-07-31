# deploy/

These manifests are the **desired state** for warden. They are staged here for
review, but the source of truth is `jedarden/declarative-config`. Per the
cluster rule, nothing here is applied with `kubectl` directly — copy into
`declarative-config` at `k8s/rs-manager/warden/`, commit, and let ArgoCD sync.

## Contents

- `namespace.yaml` — the `warden` namespace.
- `deployment.yaml` — warden Deployment (non-root, read-only rootfs, pinned semver image).
- `service.yaml` — ClusterIP.
- `ingressroute.yaml` — Certificate + Traefik IngressRoute on the `vpn` entrypoint (tailnet-only; `websecure` would make it public).
- `warden-spot-credentials-externalsecret.yaml` — Spot refresh token from OpenBao (already populated).
- `warden-caller-tokens-externalsecret.yaml` — caller bearer token(s) from OpenBao (**not yet populated**).
- `docker-hub-registry-externalsecret.yaml` — image pull credentials from OpenBao (**not yet populated**).
- `warden-build.workflowtemplate.yaml` — Argo WorkflowTemplate; also belongs in `declarative-config` at `k8s/iad-ci/argo-workflows/`.

rs-manager has no SealedSecrets controller — every secret here is an
`ExternalSecret` sourced from rs-manager's own in-cluster OpenBao via the
`openbao` ClusterSecretStore, matching the pattern every other app on this
cluster uses (see `armor`, `traefik-forward-auth`, etc. in declarative-config).

## Bring-up order

1. ~~Create the dedicated Spot org + a node pool~~ — done. The real pool is
   `ch.vs1.large-ord` on the `agent-sandbox` cloudspace (us-central-ord-1),
   which `deployment.yaml`'s `WARDEN_ALLOWED_SERVER_CLASSES` now matches.
2. ~~Build the image~~ — done, `ronaldraygun/warden:0.1.0` via the `warden-build` WorkflowTemplate.
3. Populate the two not-yet-stored OpenBao paths:
   ```bash
   bao kv put secret/rs-manager/warden/caller-tokens token="$(openssl rand -hex 32)"
   bao kv put secret/rs-manager/warden/docker/pull username="..." PAT="..."
   ```
   (run against rs-manager's OpenBao — see rs-manager/CLAUDE.md's exec-into-pod pattern)
4. Copy all manifests in this directory to `declarative-config` at `k8s/rs-manager/warden/`, commit, push — ArgoCD syncs with its own in-cluster credentials, no cluster admin access needed for this step.
5. Confirm the ExternalSecrets resolve (they'll error until step 3 is done) and the pod comes up healthy.
6. Give the generated caller token to the autoscaler/dispatcher out-of-band (its own secret) — but do not point it at warden yet; that's a separate, deliberate step pending validation.
