# Caller tokens — provisioning, rotation & revocation runbook

`security-model.md` ("Caller authentication") says callers present
`Authorization: Bearer <token>` against a sha256-matched allowlist, and that
this shared-secret scheme is a stopgap until SEAM per-agent identity lands.
This is the lifecycle that makes the allowlist real: where the tokens come
from, how they reach the store without ever becoming copyable text, how warden
consumes them, and how they are rotated, revoked, and rolled back. Until now
that lifecycle was undocumented (bead `warden-57bc080d`).

**Live state (verified 2026-09-17):** the tokens are stored at OpenBao path
`secret/rs-manager/warden/caller-tokens` (rs-manager instance), field `token`,
**version 1** (stored 2026-07-30 at bring-up; never rotated since).
ExternalSecret `warden-caller-tokens` in namespace `warden` reports
`SecretSynced=True` (refreshInterval 1h), warden serves `/healthz` → 200,
`GET /v1/pools` unauthenticated → 401 (auth middleware live), and the audit
log carries 12-hex-char caller fingerprints with no token-length hex anywhere
in the pod logs. A token has not yet been handed to any consumer — the
autoscaler/dispatcher handoff is still pending (`deploy/README.md`).

## What a caller token is — and its blast radius

- It is a **locally generated** random string (256-bit, `openssl rand -hex 32`)
  with no meaning outside warden. Nothing at Rackspace knows it; it is not a
  Spot credential. Of the ranked secret-source rules this is case 3 (generated
  under the provisioning identity and piped straight into the store).
- Possessing it lets a caller **list the pools of the `apexalgo-agent` org**
  and **scale them within the envelope** (class allowlist, bid cap, org-wide
  node ceiling). It cannot create or delete pools (no endpoint exists), cannot
  read or influence the Spot refresh token, and warden is reachable only over
  the tailnet `vpn` entrypoint.
- So exposure is **serious but bounded**: a leaked token lets an attacker move
  the worker fleet inside the envelope (cost, availability) — not breach the
  org. Contrast the Spot refresh token, whose loss compromises every
  cloudspace in the org (`spot-token-provisioning.md`). Rotate a caller token
  cheaply and on mere suspicion; the procedure costs minutes.
- The raw value must still never exist as text in a transcript, command line,
  file outside its store, commit, bead, or chat message. This runbook is
  written so that following it never produces a copy.

## Pipeline

```
openssl rand -hex 32                           agent or operator — generated at the pipe
  │  stdout → stdin, never argv, never echoed
  ▼
OpenBao (rs-manager)                           codinghome agent, write-only identity
  secret/rs-manager/warden/caller-tokens
  field: token                                 comma-separated allowlist; one field holds all of them
  ▼  ExternalSecret warden-caller-tokens (refreshInterval 1h)
  ▼  ClusterSecretStore "openbao" (kubernetes auth: SA external-secrets-rs-manager)
  ▼  K8s Secret warden-caller-tokens           declarative-config carries only this reference
  ▼  envFrom on the warden Deployment
warden process                                 reads WARDEN_CALLER_TOKENS once, at startup
  │  split on commas, trim whitespace, sha256 each token;
  │  constant-time compare of presented bearers against the digest set
  │  (internal/server/server.go)
  ▼
callers                                        Authorization: Bearer <token> on /v1/*
audit log                                      `caller` = first 12 hex chars of sha256(token)
```

The value lives in exactly **one** store. Everything downstream is a reference.

## Field format rules

- One OpenBao field, `token`, holding the whole allowlist
  **comma-separated**: `<token-a>,<token-b>`. Whitespace after commas is
  tolerated — `splitNonEmpty` trims each element (`internal/config/config.go`,
  covered by `TestCallerTokenParsing`).
- At least one non-empty element is required, or warden fails startup
  (`WARDEN_CALLER_TOKENS is required (at least one)`). An all-whitespace or
  `","` value is refused, never silently accepted.
- The same token listed twice is harmless: entries are indexed by digest, so
  duplicates collapse.
- Each entry should be a fresh `openssl rand -hex 32` (64 hex chars). The
  length is not parsed, but short human-typable strings defeat the point of a
  256-bit shared secret.

## Actors

| Step | Actor | Why |
|---|---|---|
| Generate + store into OpenBao | Agent or operator (`bao-as rs-manager-provision`) | The value is locally generated, so unlike the Spot token there is no console step an agent cannot do. The write-only identity cannot read a value back or delete. |
| OpenBao → K8s Secret | external-secrets operator | Automatic, ≤1h (`refreshInterval`); operator can force-sync (below). |
| K8s Secret → pod env | ArgoCD + kubelet | On pod (re)start only — see "Rotation". |
| Restart the pod | **Operator** | Every mutating kubectl verb is blocked for agents (org rule + PreToolUse hook), and ArgoCD does not treat a bare pod deletion as desired state. |
| Hand a token to a consumer | **Operator, out-of-band** | The autoscaler/dispatcher receives it into *its own* secret store (OpenBao path owned by that app) — never chat, never a file in a repo. |

## Provisioning (first store, or adding a caller)

**1 — Get the CAS version** (0 if the path is new):

```bash
bao-as rs-manager-provision bao kv metadata get -format=json \
  secret/rs-manager/warden/caller-tokens | jq .data.current_version
```

**2 — Store by pipe, never argv.** Generate at the pipe so the plaintext
exists only in flight:

```bash
openssl rand -hex 32 | bao-as rs-manager-provision bao kv put -cas=<n> \
  secret/rs-manager/warden/caller-tokens token=-
```

Adding a second caller (e.g. to separate audit attribution per consumer)
means re-storing the whole allowlist with the new entry appended — see
"Rotation" for the no-outage pattern.

`-cas=<n>` is **mandatory here, not just good practice**: rs-manager's mount
is `cas_required` (verified live 2026-09-17 — a write without `-cas` is
rejected with `check-and-set parameter required for this call`). Never set
`delete_version_after`; version history (mount default `max_versions=20`) is
the undo story — agents cannot delete anywhere.

**3 — Verify by property** (next section). The deliverable is the *path* and
its version number, never the value.

## Verification — by property only

Each check observes an effect of the tokens being in place; none reads one.

```bash
# OpenBao: the write landed (current_version == the n you passed to -cas)
bao-as rs-manager-provision bao kv metadata get -format=json \
  secret/rs-manager/warden/caller-tokens | jq .data.current_version

# ESO propagated it (within refreshInterval 1h): SecretSynced=True, LAST SYNC
# advanced past the moment you stored
kubectl --server=http://traefik-rs-manager:8001 \
  get externalsecret warden-caller-tokens -n warden

# warden is up, and the auth middleware still rejects unauthenticated calls
curl -sS -o /dev/null -w '%{http_code}\n' \
  https://warden-rs-manager.ardenone.com:8444/healthz          # 200
curl -sS -o /dev/null -w '%{http_code}\n' \
  https://warden-rs-manager.ardenone.com:8444/v1/pools         # 401

# audit records carry 12-char fingerprints and no token material
kubectl --server=http://traefik-rs-manager:8001 \
  logs -n warden deploy/warden | grep '"msg":"audit"'
```

**End-to-end proof** — the only check that shows a *specific caller token*
matches the live allowlist. It requires holding that token, so it belongs to
whichever actor legitimately has it in hand:

```bash
read -rs CALLER_TOKEN        # paste at the hidden prompt, then Enter
curl -sS -H "Authorization: Bearer $CALLER_TOKEN" \
  https://warden-rs-manager.ardenone.com:8444/v1/pools
unset CALLER_TOKEN
```

A 200 listing the `agent-sandbox` pool proves the whole chain for that token.
An agent that holds no caller token stops at the property checks above — and
that is the designed steady state: consumers get their token into *their*
store, not into an agent session.

**Audit correlation (operator).** To learn which allowlist entry produced the
`caller` fingerprint in an audit line, hash the token by pipe — the
fingerprint is printed, the token is not:

```bash
read -rs CALLER_TOKEN
printf '%s' "$CALLER_TOKEN" | sha256sum | cut -c1-12   # == the `caller` field
unset CALLER_TOKEN
```

## Rotation

There is no schedule — the tokens never expire. Rotate on event: suspected
exposure, a consumer being decommissioned, splitting one shared token into
per-caller tokens, or regenerating for any reason.

The single most important mechanic: **warden reads `WARDEN_CALLER_TOKENS`
once at process start** (`internal/config/config.go`). Unlike the Spot refresh
token — which at least fails late via its ~24h id_token cache — a rotation
that stops before the pod restart does *nothing at all*: no error, no log
line, the old allowlist keeps serving. Both restarts below are therefore
load-bearing, and both are operator actions.

Use the overlap pattern for zero outage (verified end-to-end against OpenBao
versioning 2026-09-17 — the version steps and rollback below ran clean on a
disposable dev server, and the multi-token field round-trips into two
allowlist entries):

1. **Store the overlap version.** Append the new token, keep the old:
   `token` field = `<old>,<new>` (whole field on one line; whitespace after
   the comma is fine), `-cas=<current+1>`. Both tokens now valid in the store.
2. **Wait for ESO** (≤1h, or operator force-sync:
   `kubectl -n warden annotate externalsecret warden-caller-tokens
   force-sync=$(date +%s) --provider-sync`) and confirm `SecretSynced=True`
   with LAST SYNC advanced.
3. **Operator: restart the workload**
   (`kubectl -n warden rollout restart deployment/warden`). The pod now
   accepts both tokens.
4. **Distribute the new token** to the consumer's own secret store,
   out-of-band. Confirm end-to-end with it (`GET /v1/pools` → 200).
5. **Store the trim version**: `token` = `<new>` only, `-cas=<current+1>`
   again.
6. **Wait for ESO, restart again.** The old token is dead the moment this pod
   comes up.

**Rollback:** the previous field content is still in OpenBao version history —
re-store it as the *next* version (`-cas=<current+1>`, never "restore in
place") and restart. There is no in-place revert and no delete; history is
the undo.

**Rolling back a consumer** that received the new token but cannot use it yet:
leave the overlap version in place (step 3 state) — both tokens work — and fix
the consumer before doing step 5. Do not trim while a consumer is broken.

## Revocation

A token is revoked by *absence*: drop it from the `token` field and restart.
There is no deny-list and nothing server-side to disable. Corollaries:

- **Leak of one of several tokens:** rotation above, minus the overlap — go
  straight to the trimmed field (other tokens untouched, so their holders see
  no outage), ESO, one restart.
- **Leak of the only token:** store a fresh token immediately (`-cas` bump),
  ESO, restart, redistribute. Callers get 401 from the restart until they are
  re-handed the new value — with no consumer provisioned yet (the current
  state) that window costs nothing.
- **"Rotate now" when exposure is only suspected:** the same procedures; the
  blast radius (above) tells you it is never an emergency on the scale of the
  Spot token. Do it promptly anyway — it is a two-command store change plus a
  restart.

**The running pod is the last place a revoked token works.** A token removed
from the field remains valid on every pod still holding the old env — that is
exactly why the restart steps exist. If a pod cannot be restarted promptly,
the operator can at least force the Secret to update (step 2) so the next
restart, whenever it happens, lands the new allowlist.

## Incident: suspected exposure

If the token ever appeared in a transcript, commit, bead, log, or chat,
treat that copy as live until the store says otherwise: rotate (above), and
record in the incident bead *that* rotation happened and the new version
number — never the value. Do not attempt to shorten this by deleting the
OpenBao path: deleting `secret/metadata/...` destroys every version
permanently, agents have no delete capability anywhere, and removing the path
leaves the ExternalSecret orphaned. Write new versions, never delete.

## Why this shape

- **OpenBao + ExternalSecret, not a SealedSecret.** rs-manager runs no
  SealedSecrets controller (`deploy/README.md`); every app on the cluster
  sources secrets from its own in-cluster OpenBao via the `openbao`
  ClusterSecretStore (kubernetes auth, role `external-secrets-rs-manager`).
- **Not in-cluster generation, despite the name.** The ranked rules prefer
  generation inside the cluster, but those routes (application-generated, or
  workflow-generated into OpenBao) buy their value for credentials an
  *application* consumes. Here the constraint is that a human-free pipeline
  can re-run provisioning, and the value never needs to exist anywhere but
  in flight and in the store — `openssl | bao kv put token=-` under the
  write-only identity is the minimal shape that satisfies both, and is the
  sanctioned case 3.
- **One field, comma-separated, instead of one field per token.** The
  ExternalSecret `data` block maps OpenBao properties to K8s Secret keys
  one-to-one; per-token fields would mean editing the manifest (a
  declarative-config commit) for every caller change. The field holds the
  whole allowlist, so consumer changes are OpenBao writes only.
- **The provisioner cannot read.** `bao-as rs-manager-provision` holds
  create/update on data and read/list on metadata only, so the act of storing
  grants no ability to exfiltrate what is stored — including these tokens.
- **Constant-time digest compare, fingerprints in audit.** Presented bearers
  are sha256-hashed and compared with `subtle.ConstantTimeCompare`
  (`internal/server/server.go`); audit lines carry the 12-hex-char fingerprint
  prefix only. Verified 2026-09-17: zero token-length (64-char) hex strings
  in the pod logs.

## Traps (each one is the shape of a real mistake)

1. **argv.** `bao kv put ... token="$(openssl rand -hex 32)"` puts the value
   in `ps`, shell history, and the transcript. Pipe it (`token=-`). This is
   the same trap as every other secret; the generation being local does not
   weaken the rule.
2. **Rotation without restart.** The K8s Secret updates and the running pod
   ignores it — warden reads env once at startup. Always pair a field change
   with a restart, and treat "SecretSynced but old token still authenticates"
   as the signature of the missing restart, not of a failed write.
3. **Restart before the Secret updated.** The inverse: if the pod restarts
   before ESO synced, it re-reads the *old* value and the rotation silently
   did not happen. Check `SecretSynced` / LAST SYNC advanced *before* the
   restart, not after.
4. **Read-back verification.** A bare `bao kv get <path>` dumps the allowlist
   to stdout and from there into the transcript. Verify by property (above).
5. **Handing tokens over chat.** The consumer's token travels into the
   consumer's own secret store, by the same pipe/`@file` rules. A token in a
   chat message is an exposure event with extra steps.
6. **Delete as cleanup.** Not available to agents by design, and for good
   reason — write the next version instead.
7. **wc -l after tr when counting tokens.** `bao kv get -field=token ... |
   tr ',' '\n' | wc -l` undercounts by one (no trailing newline) — use
   `grep -c .`. Recorded here because it produced a false alarm during this
   runbook's own verification.
