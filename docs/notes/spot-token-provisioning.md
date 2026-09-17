# Spot refresh token — provisioning & rotation runbook

`security-model.md` control 2 (credential separation) says the Rackspace Spot
refresh token "lives only inside warden". This is the chain that makes that
true: where the token comes from, how it reaches the store without ever
becoming copyable text, how warden consumes it, and how it is rotated. Until
now that chain was undocumented.

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
  delete cloudspaces across the org. The token therefore belongs to the
  dedicated `apexalgo-agent` org, which contains only worker cloudspaces —
  that is the floor (`security-model.md` control 1). `WARDEN_ORG_NAMESPACE`
  in `deploy/deployment.yaml` (`org-knyiltp8zznvkz5g`, not a secret) must name
  the same org the token belongs to.
- Loss of this value compromises every cloudspace in that org. It must never
  exist as text in a transcript, command line, file outside its store, commit,
  bead, or chat message. This runbook is written so that following it never
  produces a copy.

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
warden process                                 reads WARDEN_SPOT_REFRESH_TOKEN once, at startup
  │  exchange → 24h id_token, cached in memory (internal/spot/client.go)
  ▼
spot.rackspace.com                             list / scale(count) only
```

The value lives in exactly **one** store. Everything downstream is a reference.

## Actors

| Step | Actor | Why |
|---|---|---|
| Obtain from console | **Operator** (human with Spot login) | An agent reaching the console would have to put the token on a screen — an ADB screenshot of the console page lands the value in the agent transcript. That path is prohibited; the console step is deliberately the one step agents cannot do. |
| Store into OpenBao | Operator or agent (`bao-as rs-manager-provision`) | Write-only identity: it can create/update and read *metadata* but cannot read a value back or delete. |
| OpenBao → K8s Secret | external-secrets operator | Automatic, ≤1h. |
| K8s Secret → pod env | ArgoCD + kubelet | On pod (re)start only — see "Rotation". |
| Restart the pod | **Operator** | Every mutating kubectl verb is blocked for agents (org rule + PreToolUse hook), and ArgoCD does not treat a bare pod deletion as desired state. |

## Provisioning (first store, or a new org)

The path below is the live one; for a second org use
`secret/rs-manager/warden/rackspace-spot/<org>-organization` with field
`api-token`, and point `WARDEN_ORG_NAMESPACE` at that org.

**1 — Operator: obtain the token.** Spot Console → select the
`apexalgo-agent` org → *API Access → Terraform* → copy the refresh token.
Paste it directly into one of the two transfer shapes below — not into chat,
notes, a file in a repo, or a shell line where it would be history-recorded
(`TOKEN=paste…` is history; `read -rs` is not).

**2 — Get the CAS version** (0 if the path is new):

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
its version number, never the value.

## Verification — by property only

Each check observes an effect of the token being in place; none reads it.

```bash
# OpenBao: the write landed (current_version == the n you passed to -cas)
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
curl -sS -H "Authorization: Bearer $WARDEN_CALLER_TOKEN" \
  https://warden-rs-manager.ardenone.com:8444/v1/pools
```

A 200 listing the `agent-sandbox` pool proves the whole chain. The caller
token is a different secret (`secret/rs-manager/warden/caller-tokens`) and
travels by the same rules — materialize it into the environment of the shell
that needs it, never into a command line or file. Its own lifecycle runbook:
[`caller-token-provisioning.md`](caller-token-provisioning.md). Agents that
hold no caller
token stop at the checks above plus pod logs
(`kubectl --server=http://traefik-rs-manager:8001 logs -n warden deploy/warden`:
audit lines carry 12-char caller fingerprints, never token material) and leave
the final call to the operator.

Note what `SecretSynced` does **not** prove: it only shows OpenBao → K8s
Secret sync worked, not that the token is *valid* at Spot. Only the
`/v1/pools` call proves that. A dead token presents as: pod Running and
`/healthz` fine (neither probe touches Spot), `SecretSynced` True, and every
`/v1/pools` or scale returning 5xx with `oauth token request: status 4xx` in
the pod logs.

## Rotation

There is no schedule — Spot refresh tokens are long-lived. Rotate on event:
suspected exposure, the operator who held it leaving, org handover, or a
console regeneration for any reason.

1. **Operator: regenerate in console** (same page: *API Access → Terraform*).
   Whether reissuing immediately invalidates the old token is not documented
   by Rackspace; treat the old value as burned from this moment and keep the
   window short.
2. **Store as a new version** — steps 2–3 above with the fresh value and
   `current_version + 1` as `-cas`. Do not delete or "clean up" the old
   version; history is the rollback.
3. **Wait for ESO** (≤1h) and confirm by property: ExternalSecret `LAST SYNC`
   advanced and the Secret's `resourceVersion` moved.
4. **Operator: restart the workload.** warden reads
   `WARDEN_SPOT_REFRESH_TOKEN` from env exactly once at process start
   (`internal/config/config.go`) and caches the derived id_token for ~24h with
   no reload path — an updated Secret does nothing to a running pod. The
   restart is the one manual cluster action in the chain
   (`kubectl -n warden rollout restart deployment/warden` from an admin
   context; agents are blocked from it by rule, not just by policy).
5. **Verify end-to-end** (`GET /v1/pools` → 200) and confirm version `n+1` in
   OpenBao metadata.

**Outage window:** from regeneration until the restart in step 4, the running
pod still holds the old refresh token. Because the cached id_token lives ~24h,
list/scale keep working after a revocation until the first exchange attempt
after cache expiry — the failure surfaces late and all at once. Do the restart
promptly after the Secret updates rather than letting the cache decide when
the outage starts. If the restart is delayed past cache expiry, symptoms are
exactly the "dead token" signature above.

**Rollback:** the previous token version is still in OpenBao history (unless
the console invalidation was immediate and hard) — re-store it as the next
version and restart again. If the console genuinely invalidates old tokens on
reissue, there is no rollback and step 4 becomes urgent.

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
