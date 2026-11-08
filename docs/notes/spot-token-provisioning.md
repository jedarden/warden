# Spot refresh token — provisioning & rotation runbook

`security-model.md` control 2 (credential separation) says the Rackspace Spot
refresh token "lives only inside warden". This is the chain that makes that
true: where the token comes from, how it reaches the store without ever
becoming copyable text, how warden consumes it, and how it is rotated. The
existing deployment is single-target. The target-mode procedure below is for
a future, deliberate GitOps change; it does not provision or enable another
account by itself.

**Live state (verified 2026-09-16):** the token is stored at OpenBao path
`secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization`, field
`api-token`, version 1 (stored 2026-07-27 during the live-API probe, see
`docs/research/rackspace-spot-api.md`). ExternalSecret
`warden-spot-credentials` in namespace `warden` on rs-manager reports
`SecretSynced`, and warden serves `GET /healthz` → 200 on
`https://warden-rs-manager.ardenone.com:8444`.

## The token and its blast radius

- It is a **refresh token** (long-lived) issued per user in the Spot Console →
  *API Access → Terraform*. It is not the short-lived bearer: warden exchanges
  it for a 24h OIDC `id_token` at `login.spot.rackspace.com/oauth/token`
  in-process and bears that (see `docs/research/rackspace-spot-api.md` — the
  API wants `id_token`, not `access_token`).
- Spot IAM is **org-scoped and coarse**: a token that can scale a pool can also
  delete cloudspaces across an org it can reach. The currently deployed token
  belongs to the dedicated `apexalgo-agent` org, which contains only worker
  cloudspaces — that is the floor (`security-model.md` control 1). In legacy
  mode, `WARDEN_ORG_NAMESPACE` in `deploy/deployment.yaml`
  (`org-knyiltp8zznvkz5g`, not a secret) names that org. In target mode each
  target names its own org, and the account credential must have access to it.
- Loss of this value compromises every cloudspace in every org the account
  credential can reach. It must never exist as text in a transcript, command
  line, file outside its store, commit, bead, or chat message. This runbook is
  written so that following it never produces a copy.

## Pipeline

```
Spot Console (API Access → Terraform)          operator only — see "Actors"
  │  paste into a shell var via `read -rs`, or a mode-600 tmpfs file
  ▼
OpenBao (rs-manager)                           codinghome agent, write-only identity
  secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization
  field: api-token                             value enters by pipe/stdin or @file, never argv
  ▼  ExternalSecret warden-spot-credentials (refreshInterval 1h)
  ▼  ClusterSecretStore "openbao" (kubernetes auth: SA external-secrets-rs-manager)
  ▼  K8s Secret warden-spot-credentials        declarative-config carries only this reference
  ▼  envFrom on the warden Deployment
warden process                                 reads WARDEN_SPOT_REFRESH_TOKEN in legacy mode,
                                               or one WARDEN_SPOT_REFRESH_TOKEN_<ALIAS>
                                               per account in target mode, at startup
  │  exchange → 24h id_token, cached in memory (internal/spot/client.go)
  ▼
spot.rackspace.com                             list / scale(count) only
```

OpenBao is the authoritative store. External Secrets and pod environments
hold derived copies; Git carries only references.

## Actors

| Step | Actor | Why |
|---|---|---|
| Obtain from console | **Operator** (human with Spot login) | An agent reaching the console would have to put the token on a screen — an ADB screenshot of the console page lands the value in the agent transcript. That path is prohibited; the console step is deliberately the one step agents cannot do. |
| Store into OpenBao | Operator or agent (`bao-as rs-manager-provision`) | Write-only identity: it can create/update and read *metadata* but cannot read a value back or delete. |
| OpenBao → K8s Secret | external-secrets operator | Automatic, ≤1h. |
| K8s Secret → pod env | ArgoCD + kubelet | On pod (re)start only — see "Rotation". |
| Restart the pod | GitOps change author | Change the Deployment pod-template annotation in `declarative-config` and let ArgoCD reconcile it. Direct `kubectl rollout restart` is prohibited for ArgoCD-managed resources. |

## Legacy single-target provisioning

The path below is the live credential for the one deployed caller target. Do
not change `WARDEN_ORG_NAMESPACE` to add an org: it replaces the only legacy
target. To serve another org, follow **Target-mode provisioning** below. An
org on the same Spot account may reuse the account's token; a separate account
needs its own token.

**1 — Operator: obtain the token.** Spot Console → select the
`apexalgo-agent` org → *API Access → Terraform* → copy the refresh token.
Paste it directly into one of the two transfer shapes below — not into chat,
notes, a file in a repo, or a shell line where it would be history-recorded
(`TOKEN=paste…` is history; `read -rs` is not).

**2 — Get the CAS version** (0 if the path does not yet exist; a metadata 404
is expected only for a new path):

```bash
bao-as rs-manager-provision bao kv metadata get -format=json \
  secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization \
  | jq .data.current_version
```

**3 — Store by pipe or file, never argv.** Either:

```bash
# a) stdin: paste at the hidden prompt, then Enter — the value is never echoed,
#    never in history, never in argv
read -rs SPOT_REFRESH_TOKEN
printf '%s' "$SPOT_REFRESH_TOKEN" \
  | bao-as rs-manager-provision bao kv put -cas=<n> \
      secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization api-token=-
unset SPOT_REFRESH_TOKEN
```

```bash
# b) mode-600 file on tmpfs, consumed by @file
install -m 600 /dev/null /run/user/$UID/spot.rt && $EDITOR /run/user/$UID/spot.rt
bao-as rs-manager-provision bao kv put -cas=<n> \
  secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization \
  api-token=@/run/user/$UID/spot.rt
shred -u /run/user/$UID/spot.rt
```

`-cas=<n>` is mandatory practice on every write — and on rs-manager it is
also *enforced*: the mount is `cas_required` (verified live 2026-09-17 — a
write without `-cas` is rejected with `check-and-set parameter required for
this call`; this note previously said "not yet", which had gone stale). Never
set `delete_version_after`; version history (mount default `max_versions=20`)
is the undo story — agents cannot delete anywhere.

**4 — Verify by property** (next section). The deliverable is the *path* and
its version number, never the value. A CAS write at version `n` creates
version `n+1`; pass the **current** version, not `n+1`, to `-cas`.

## Verification — by property only

Each check observes an effect of the token being in place; none reads it.

```bash
# OpenBao: the write landed (current_version == the n passed to -cas, plus 1)
bao-as rs-manager-provision bao kv metadata get -format=json \
  secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization \
  | jq .data.current_version

# ESO propagated it (within refreshInterval 1h): SecretSynced=True, LAST SYNC
# advanced past the moment you stored
kubectl --server=http://traefik-rs-manager:8001 \
  get externalsecret warden-spot-credentials -n warden

# the K8s Secret was rewritten (resourceVersion moved — do not print data)
kubectl --server=http://traefik-rs-manager:8001 \
  get secret warden-spot-credentials -n warden \
  -o jsonpath='{.metadata.resourceVersion}{"\n"}'

# warden is up
curl -sS https://warden-rs-manager.ardenone.com:8444/healthz
```

**End-to-end proof** — the only check that shows warden's *copy* of the token
actually exchanges against the live Spot API:

```bash
(
  read -rs WARDEN_CALLER_TOKEN  # paste at the hidden prompt, then Enter
  printf 'Authorization: Bearer %s\n' "$WARDEN_CALLER_TOKEN" |
    curl -sS --header @- \
      https://warden-rs-manager.ardenone.com:8444/v1/pools
)
```

A 200 listing the `agent-sandbox` pool proves the whole chain. The caller
token is a different secret (`secret/rs-manager/warden/caller-tokens`) and
travels by the same rules — enter it only at the hidden prompt and pipe it to
curl's stdin header, never into a command line or file. Its own lifecycle runbook:
[`caller-token-provisioning.md`](caller-token-provisioning.md). Agents that
hold no caller
token stop at the checks above plus pod logs
(`kubectl --server=http://traefik-rs-manager:8001 logs -n warden deploy/warden`:
audit lines carry 12-char caller fingerprints, never token material) and leave
the final call to the operator.

Note what `SecretSynced` does **not** prove: it only shows OpenBao → K8s
Secret sync worked, not that the token is *valid* at Spot. Only an
authenticated list (`/v1/pools` in legacy mode, a scoped list in target mode)
proves that. A dead token presents as: pod Running and `/healthz` fine
(neither probe touches Spot), `SecretSynced` True, and list or scale calls
returning 5xx with `oauth token request: status 4xx` in the pod logs.

## Target-mode provisioning (future change)

Use this procedure when the approved target set changes. `declarative-config`
is the source of truth: edit `k8s/rs-manager/warden/warden-spot-credentials-externalsecret.yaml`
and `k8s/rs-manager/warden/deployment.yaml` there, commit on `main`, push to
Forgejo, and let ArgoCD Application `warden-ns-rs-manager` sync. The `deploy/`
copies in this repo are reference mirrors, not a second deployment path. Do
not create credentials or switch modes merely to exercise this runbook.

1. **Choose account aliases and map access.** `account` is a local, stable
   lowercase alias (for example `agent` or `operations`), not a Spot org
   namespace. Give each Spot account exactly one canonical alias and one
   `WARDEN_SPOT_REFRESH_TOKEN_<ALIAS>` name. Use `AGENT` for `agent`,
   `OPERATIONS` for `operations`; write an alias containing hyphens with
   underscores in the env name. Every org reachable with the same account
   credential repeats that *same* alias and env reference. Distinct accounts
   must use distinct aliases and env names. A second org on the `agent`
   account needs no second token. Confirm its access with the scoped list
   after rollout. If it is a different Spot account, provision a different
   token first. If the main-org price guard's `apexalgo` account ever becomes
   a caller target, reuse its existing `WARDEN_SPOT_REFRESH_TOKEN_APEXALGO`
   env key; do not add a second key for that account or change the guard's
   reference independently.
2. **Store one token per new account in the owning OpenBao instance.** The
   existing `agent` credential is already at
   `secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization`, field
   `api-token`; its target-mode alias can point at this path without copying
   the value. For a new `operations` account, use a distinct path such as
   `secret/rs-manager/warden/rackspace-spot/operations-account`, field
   `api-token`. Obtain its refresh token through the Spot Console as in the
   legacy steps above. On **rs-manager**, get metadata for that path (404
   means CAS version 0), then enter the value at the hidden prompt. Replace
   `<current>` with the metadata version, or 0 for a new path:

   ```bash
   bao-as rs-manager-provision bao kv metadata get -format=json \
     secret/rs-manager/warden/rackspace-spot/operations-account |
     jq .data.current_version
   (
     set +x
     set -euo pipefail
     read -rs SPOT_REFRESH_TOKEN
     printf '%s' "$SPOT_REFRESH_TOKEN" |
       bao-as rs-manager-provision bao kv put -cas=<current> \
         secret/rs-manager/warden/rackspace-spot/operations-account api-token=-
   )
   ```

   Never put the value in an argument or manifest. Verify only
   `bao kv metadata get -format=json` and the new version number.
3. **Add one ExternalSecret data mapping per alias.** In the authoritative
   `warden-spot-credentials-externalsecret.yaml`, keep the legacy key until
   rollback to single-target mode is no longer needed. Add mappings like:

   ```yaml
   spec:
     data:
       - secretKey: WARDEN_SPOT_REFRESH_TOKEN_AGENT
         remoteRef:
           key: rs-manager/warden/rackspace-spot/apexalgo-agent-organization
           property: api-token
       - secretKey: WARDEN_SPOT_REFRESH_TOKEN_OPERATIONS
         remoteRef:
           key: rs-manager/warden/rackspace-spot/operations-account
           property: api-token
   ```

   These are `spec.data` **entries to add**, not a replacement for the
   manifest's API version, store reference, target, or existing data. The
   `remoteRef.key` omits the `secret/` KV mount prefix, as the deployed
   ExternalSecret does. For an alias already injected by another `envFrom`
   Secret (the guard's `APEXALGO` key), reuse that key and do not duplicate
   it in `warden-spot-credentials`.
4. **Wire the Deployment.** Keep its `envFrom` reference to
   `warden-spot-credentials` (and to `warden-spot-guard` if the guard is
   enabled). Add `WARDEN_TARGETS_JSON` under the warden container's `env`.
   For example, after replacing the illustrative org IDs with verified IDs:

   ```yaml
   - name: WARDEN_TARGETS_JSON
     value: >-
       [{"account":"agent","namespace":"org-knyiltp8zznvkz5g","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_AGENT","allowScale":true,"maxTotalNodes":10,"allowedServerClasses":["ch.vs1.large-ord"],"maxBidPrice":0.01},
        {"account":"agent","namespace":"org-example-shared","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_AGENT"},
        {"account":"operations","namespace":"org-example-operations","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_OPERATIONS"}]
   ```

   The first entry preserves the current target's scaling envelope; new
   entries default to read-only because `allowScale` defaults to false. If
   scaling is intended for another org, declare its own `maxTotalNodes`,
   `allowedServerClasses`, and `maxBidPrice`. The old
   `WARDEN_ORG_NAMESPACE`, `WARDEN_SPOT_REFRESH_TOKEN`, and legacy envelope
   values are ignored by caller routing in target mode, but retaining them
   lets a later GitOps rollback restore single-target mode. Check any
   `WARDEN_SPOT_GUARD_JSON` account binding against the target set; if a guard
   account is also a caller account, both must use the same env reference.
5. **Validate references before commit.** This check reads the proposed
   Deployment manifest and prints only `true` on success. It rejects missing
   or multiple target values, unknown JSON fields (including `token`,
   `refreshToken`, and `secretPath`), and a `refreshTokenEnv` that is anything
   other than an env name. Review the diff for literal credential values and
   OpenBao paths inside `WARDEN_TARGETS_JSON`; paths belong only in
   ExternalSecret `remoteRef.key`. The Go config loader also rejects unknown
   fields, duplicate orgs, missing env vars, and mismatched account/env pairs
   at startup.

   ```bash
   set -o pipefail
   yq -r '.spec.template.spec.containers[] | select(.name == "warden") |
     .env[] | select(.name == "WARDEN_TARGETS_JSON") | .value' \
     k8s/rs-manager/warden/deployment.yaml |
     jq -s -e 'length == 1 and (.[0] |
       type == "array" and length > 0 and
       all(.[]; type == "object" and
         ((keys - ["account", "namespace", "refreshTokenEnv", "allowScale",
                   "maxTotalNodes", "allowedServerClasses", "maxBidPrice"]) | length == 0) and
         (.account | type == "string") and
         (.namespace | type == "string" and startswith("org-")) and
         (.refreshTokenEnv | type == "string" and
           test("^WARDEN_SPOT_REFRESH_TOKEN_[A-Z0-9_]+$"))))'
   ```

6. **Sync and prove each target.** After the GitOps change, wait for
   `warden-ns-rs-manager` to be Synced and Healthy. Confirm
   `warden-spot-credentials` has `SecretSynced=True`, and check only the
   resulting Secret's `metadata.resourceVersion` (never `data`). Confirm the
   Deployment rollout completes. Ask an authorized caller to make one bounded,
   authenticated scoped list per `(account, namespace)`, including both orgs
   sharing an alias; a successful `/healthz` or `/v1/accounts` alone does not
   prove Spot accepts that token for each org. Use the same hidden caller-token
   input as above, substituting the actual account and org:

   ```bash
   (
     read -rs WARDEN_CALLER_TOKEN
     printf 'Authorization: Bearer %s\n' "$WARDEN_CALLER_TOKEN" |
       curl --fail --silent --show-error --max-time 30 --header @- \
         'https://warden-rs-manager.ardenone.com:8444/v1/accounts/agent/organizations/org-knyiltp8zznvkz5g/pools' \
         >/dev/null
   )
   ```

   Repeat with each configured pair. A nonzero exit needs investigation
   before permitting scaling; do not use a scale call as the credential test.

## Rotation and restart

There is no schedule — Spot refresh tokens are long-lived. Rotate on event:
suspected exposure, the operator who held it leaving, org handover, or a
console regeneration for any reason. Rotate **one account alias at a time**.
All org targets sharing that alias use the same refreshed credential; separate
accounts keep their own versions and references.

1. **Operator: regenerate that account's token in the console** (same page:
   *API Access → Terraform*). Whether reissuing immediately invalidates the
   old token is not documented by Rackspace; treat the old value as burned
   from this moment and keep the window short.
2. **Write the new version at that alias's OpenBao path** using the hidden
   prompt and stdin pipe above. Get `current_version` from metadata and pass
   that number as `-cas`; the new version is `current_version + 1`. Keep the
   previous version until the new one is verified. Do not change another
   account's path or duplicate the value into another alias.
3. **Wait for ESO** (refresh interval 1h) and confirm by property:
   `SecretSynced=True`, `LAST SYNC` advanced, and the K8s Secret's
   `resourceVersion` moved. A synced Secret alone does not restart a pod.
4. **Restart through GitOps.** Warden reads its token env vars once at process
   start (`internal/config/config.go`) and caches the derived id_token for
   about 24h. In `declarative-config/k8s/rs-manager/warden/deployment.yaml`,
   change a pod-template annotation for the rotated alias, for example
   `warden.ardenone.com/spot-token-operations-version: "2"` under
   `spec.template.metadata.annotations`. Use the actual OpenBao version, not
   a secret value. Commit and push the manifest to Forgejo; let ArgoCD sync
   `warden-ns-rs-manager`. The changed pod template triggers a Deployment
   rollout, with `maxSurge: 0`/`maxUnavailable: 1` so the price guard never
   has two writers. Never mutate the managed Deployment or pod with `kubectl`.
5. **Verify the new pod and the affected orgs.** Check rollout completion,
   metadata version `n+1`, ESO status, and one bounded authenticated list for
   **each** org using the rotated alias (scoped route in target mode,
   `/v1/pools` in legacy mode). A 200 from `/healthz` alone cannot prove the
   credential works. Confirm unaffected aliases remain mapped to their own
   env names and paths.

   ```bash
   bao-as rs-manager-provision bao kv metadata get -format=json \
     secret/rs-manager/warden/rackspace-spot/operations-account |
     jq .data.current_version
   kubectl --server=http://traefik-rs-manager:8001 -n warden \
     get externalsecret warden-spot-credentials
   kubectl --server=http://traefik-rs-manager:8001 -n warden \
     get secret warden-spot-credentials \
     -o jsonpath='{.metadata.resourceVersion}{"\n"}'
   kubectl --server=http://traefik-rs-manager:8001 -n warden \
     rollout status deployment/warden --timeout=120s
   ```

   Replace the example path with the actual alias path. For the existing
   guard-only `APEXALGO` alias, inspect `warden-spot-guard` instead of
   `warden-spot-credentials`; its env key is in that ExternalSecret.

**Outage window:** from regeneration until the restart in step 4, the running
pod still holds the old refresh token. Because the cached id_token lives ~24h,
list/scale keep working after a revocation until the first exchange attempt
after cache expiry — the failure surfaces late and all at once. Do the restart
promptly after the Secret updates rather than letting the cache decide when
the outage starts. If the restart is delayed past cache expiry, symptoms are
exactly the "dead token" signature above.

**Rollback one alias:** If the prior token is still valid, an actor with
read permission can stream its prior OpenBao version into a new current
version without displaying it. First obtain the current version from metadata;
substitute the prior version and current CAS below. Use the path belonging to
the affected alias only. The read identity's output is captured in a
short-lived subshell variable, checked for a non-empty value, then piped to
the write-only identity. Never run the `kv get` half alone or pipe it to a
logger.

```bash
(
  set +x
  set -euo pipefail
  prior_token="$(bao-as rs-manager bao kv get -field=api-token -version=<prior> \
    secret/rs-manager/warden/rackspace-spot/operations-account)"
  test -n "$prior_token"
  printf '%s' "$prior_token" |
    bao-as rs-manager-provision bao kv put -cas=<current> \
      secret/rs-manager/warden/rackspace-spot/operations-account api-token=-
)
```

Verify metadata and ESO as above, then advance that alias's pod-template
annotation to the **new** OpenBao version and let ArgoCD roll the pod. Recheck
every org sharing the alias. Do not point the annotation backward: it is a
rollout marker, not a selector for an older Secret version. If Spot invalidated
the prior token, obtain a fresh valid one from the console and write it as a
new version; OpenBao history cannot undo Spot revocation.

**Rollback target-mode configuration:** If the target list or Secret mapping
is wrong, restore the last known-good desired-state values in a new
`declarative-config` commit and push it. To return to legacy mode, remove
`WARDEN_TARGETS_JSON` and ensure the legacy
`WARDEN_SPOT_REFRESH_TOKEN` Secret key, `WARDEN_ORG_NAMESPACE`, and envelope
still exist. ArgoCD reconciles and rolls the changed pod template. Verify
`SecretSynced`, rollout completion, and an authenticated legacy `/v1/pools`
list. This changes caller routing; it does not reverse a Spot token revocation.

## Incident: suspected exposure

Regenerate in the console first (that is the actual revocation), then run the
rotation above. The value cannot be "unseen" — if it ever appeared in a
transcript, commit, bead, or log, treat that copy as live until the console
says otherwise. Do not attempt to shorten this by deleting the OpenBao path:
deleting `secret/metadata/...` destroys every version permanently, agents have
no delete capability anywhere, and removing the path leaves the ExternalSecret
orphaned. Write new versions, never delete.

## Why this shape

- **OpenBao + ExternalSecret, not a SealedSecret.** rs-manager runs no
  SealedSecrets controller (`deploy/README.md`); every app on the cluster
  sources secrets from its own in-cluster OpenBao via the `openbao`
  ClusterSecretStore (kubernetes auth, role `external-secrets-rs-manager`).
  Earlier doc drafts credited a SealedSecret; `security-model.md` and
  `docs/plan/plan.md` were corrected 2026-09-16 (commit `e4b9204`).
- **In-cluster generation doesn't apply.** The value originates at the Spot
  Console, outside the cluster — of the ranked secret-source rules this is
  case 3 (third-party credential stored via the provisioning identity), not
  case 1 or 2 (operator/workflow-generated).
- **declarative-config carries only the reference.**
  `deploy/warden-spot-credentials-externalsecret.yaml` (mirrored at
  `k8s/rs-manager/warden/` in declarative-config) names the path and property;
  the value exists only in OpenBao and its two derived copies (K8s Secret, pod
  env).
- **The provisioner cannot read.** `bao-as rs-manager-provision` holds
  create/update on data and read/list on metadata only, so the act of storing
  grants no ability to exfiltrate what is stored.

## Traps (each one has actually bitten this fleet)

1. **Console via ADB screenshot.** The phone renders the console page fine —
   and the screenshot then contains the token and lands in the transcript. The
   console step is operator-only; do not route around it.
2. **argv.** `bao kv put ... token="$(openssl rand -hex 32)"` puts the value in
   `ps`, shell history, and the transcript. Pipe it (`key=-`) or use `@file`.
3. **Read-back verification.** A bare `bao kv get <path>` dumps the value to
   stdout and from there into the transcript. Verify by property (above).
4. **A value in declarative-config, a bead, a commit, or chat.** The manifest
   carries the reference; the bead records the *path* and version number.
5. **Delete as cleanup.** Not available to agents by design, and for good
   reason — write the next version instead.
