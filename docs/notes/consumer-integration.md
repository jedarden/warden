# Consumer integration — handing a caller token to the autoscaler/dispatcher

`caller-token-provisioning.md` documents the server half: how caller tokens
are generated into OpenBao, how warden consumes the allowlist, and how that
allowlist is rotated and rolled back. It ends with an open item — *"a token
has not yet been handed to any consumer; the autoscaler/dispatcher handoff is
still pending"* (bead `warden-1d182474`). This note is the consumer half:
what a consumer embeds, how its token reaches it by reference, and the
rollout / rotation / revocation / rollback lifecycle from the consumer's
side. The server-half procedures are not repeated here — they are linked.

## Status

- **Landed (this repo):** `internal/intent` — the client package an
  autoscaler or the NEEDLE dispatcher embeds — and `cmd/wardenctl`, the
  reference consumer binary. Both tested against warden's exact wire contract
  (`docs/notes/api.md`); `wardenctl unauth-check` verified live against the
  rs-manager deployment (401 as expected, 2026-11-08).
- **Not landed, and deliberately not improvised here:** the deployed consumer
  itself. Per the human decision recorded on `warden-1d182474` (2026-10-03),
  before any credential is staged a concrete consumer owner must be selected
  with its demand signal, deterministic desired-capacity algorithm, pool
  ownership, retry/failure behavior, and scale-to-zero/preemption safety —
  and the controller location/topology question in `docs/plan/plan.md` is
  still open. Building an autoscaler policy on an invented demand signal is
  exactly the speculative work the fleet policy forbids. When a consumer is
  selected, everything below applies to it unchanged; the code it needs
  already exists.

## What a consumer embeds

`internal/intent` (package doc has the full contract):

- **One token, by reference.** The token enters the process as
  `WARDEN_CALLER_TOKEN` (env) or a mode-0600 file (`wardenctl -token-file`).
  There is no API or flag that accepts the value on argv — argv is captured
  in transcripts, shell history, and `ps`.
- **`Authorization: Bearer <token>` on every `/v1` request**, set in one
  place (`Client.do`). The raw value never reaches a URL, a body, a log
  field, or an error string.
- **Fail closed.** Any authenticated operation with no token configured
  returns `ErrNoToken` before a request is built. A consumer never silently
  falls back to unauthenticated calls.
- **The error contract as sentinels** — `ErrUnauthorized` (401),
  `*intent.Denial` (403, with warden's reason), `ErrNotFound` (404),
  `ErrConflict` (409), `ErrUpstream` (502) — so a dispatcher programs against
  outcomes, not status-string matching.
- **A bounded scale path.** `ScaleWithinWindow` re-lists, refuses any count
  outside the pool's live `[lowerBound, upperBound]` window client-side
  (`ErrOutOfWindow`, nothing sent, nothing audited), and only then calls
  `ScalePool`. The server-side envelope stays authoritative; the client guard
  keeps a mis-programmed dispatcher from spending audit lines on certain
  denials.
- **An audit identity, not the token.** `intent.Fingerprint(token)` is the
  first 12 hex chars of sha256(token) — byte-identical to the `caller` field
  warden writes to its audit log (`internal/server/server.go`). A consumer
  logs the fingerprint; an operator matches it to warden's audit lines;
  nobody holds copyable token material to do it.
- **An auth-boundary proof.** `CheckAuth` asserts both halves read-only: the
  same `GET /v1/pools` *without* credentials must be rejected 401, and *with*
  the configured token must succeed 200. A server that answers 200 to the
  unauthenticated half fails the check — a consumer must never conclude "auth
  works" from a server that isn't enforcing it. `CheckUnauthenticatedRejected`
  is the credential-less half alone and needs no token.

`cmd/wardenctl` is the executable form: `pools`, `accounts`, `scale POOL
COUNT` (window-guarded), `auth-check`, `unauth-check`. Exit codes: 0 success,
1 operational failure, 2 usage. Output is JSON, so a dispatcher can shell out
during bring-up and then link the package.

## The secret reference

A consumer never stores a token itself; it references one. The pattern
mirrors warden's own (`deploy/warden-caller-tokens-externalsecret.yaml`), but
pointed at a **path the consumer owns**, so audit attribution, rotation, and
revocation are per-consumer:

```
OpenBao  secret/<cluster>/<consumer>/warden-caller-token   field: token
   │  (provisioned per the rules in caller-token-provisioning.md — pipe/@file,
   │   write-only identity, -cas, never argv, never a read-back)
   ▼
ExternalSecret  <consumer>-warden-caller-token   (consumer's namespace)
   ▼
Secret  <consumer>-warden-caller-token  key: WARDEN_CALLER_TOKEN
   ▼
Deployment envFrom.secretRef  →  WARDEN_CALLER_TOKEN  →  internal/intent
```

The same raw value is *also* an entry in warden's allowlist
(`secret/rs-manager/warden/caller-tokens`, comma-separated field) — added by
the overlap pattern during rollout and trimmed during revocation. The value
lives in exactly two stores: the consumer's own path and warden's allowlist.
Everything else — manifests, Git, env files, argv — holds references only.

The ExternalSecret body is the same shape as warden's, with the consumer's
path and key:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: <consumer>-warden-caller-token
  namespace: <consumer-ns>
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: openbao
    kind: ClusterSecretStore
  target:
    name: <consumer>-warden-caller-token
    creationPolicy: Owner
  data:
    - secretKey: WARDEN_CALLER_TOKEN
      remoteRef:
        key: <cluster>/<consumer>/warden-caller-token
        property: token
```

## Rollout (the handoff itself)

Every step is either GitOps (declarative-config commit → ArgoCD sync) or a
by-pipe OpenBao write. The order is load-bearing: code and deployment
reference first, credential last — "no credential should be staged until
executable consumer code and its deployment reference exist".

1. **Deploy the consumer** with the ExternalSecret above (its store path may
   be empty at this point — the ExternalSecret will report
   `SecretSynced=False` until step 4; make readiness not depend on it, or
   deploy after step 4). Configure `WARDEN_URL` (e.g.
   `https://warden-rs-manager.ardenone.com:8444`).
2. **Prove the boundary first, without any credential** — from the consumer's
   context (or anywhere): `wardenctl unauth-check` → `401 as expected`. This
   is the acceptance's unauthenticated-401 check, and it needs no secret.
3. **Stage the overlap allowlist.** Generate the consumer's distinct token
   and append it to warden's allowlist in one protected context — the
   rotation subshell in `caller-token-provisioning.md` step 2, whose
   "if a consumer must receive it" comment is exactly this: pipe the new
   value to the consumer's own store *before the subshell ends*:

   ```bash
   (
     set -eu
     current="$(bao-as rs-manager bao kv get -field=token \
       secret/rs-manager/warden/caller-tokens)"
     new="$(openssl rand -hex 32)"
     test -n "$current" && test -n "$new"
     printf '%s' "$new" |
       bao-as <cluster>-provision bao kv put -cas=0 \
         secret/<cluster>/<consumer>/warden-caller-token token=-
     printf '%s,%s' "$current" "$new" |
       bao-as rs-manager-provision bao kv put -cas=<current> \
         secret/rs-manager/warden/caller-tokens token=-
   )
   ```

   Neither value is printed or passed as an argument; each lands in exactly
   its own store. Verify by property only (metadata `current_version`, then
   `SecretSynced=True` on both ExternalSecrets) — never a read-back.
4. **Wait for both ExternalSecrets** (`warden-caller-tokens` and the
   consumer's) to report `SecretSynced=True` with LAST SYNC advanced. The
   refresh interval is 1h; an operator can force-sync (see the runbook).
   Order matters: the warden pod must not restart before its Secret synced,
   or it re-reads the old allowlist and the rotation silently didn't happen.
5. **Restart warden** — it reads `WARDEN_CALLER_TOKENS` once at start. The
   operator path is `kubectl -n warden rollout restart deployment/warden`;
   the GitOps path is a declarative-config commit that changes the Deployment
   pod template (e.g. bump an allowlist-version annotation), letting ArgoCD
   do the rollout. Agents have no mutating kubectl; the GitOps path is theirs.
6. **Consumer-observed proof:** `wardenctl auth-check` from the consumer's
   context → prints the 401/200 pair and the `caller` fingerprint. This is
   the acceptance's authenticated-list check, observed by the consumer
   itself. Correlate: the printed fingerprint must equal the `caller` field
   of the audit line warden just wrote (`kubectl logs -n warden deploy/warden
   | grep '"msg":"audit"'`).
7. **Bounded scale/restore exercise** against the designated worker pool —
   read-only plus two window-guarded writes, the smallest motion that proves
   the whole chain (see below).
8. **Record** the consumer, its store path, and the allowlist version on the
   owning bead (paths and versions, never values). Superseded access (e.g. a
   shared token the consumer replaced) is removed by the trim step of
   rotation or by revocation.

### The bounded scale/restore exercise

The designated pool must be a **worker pool in the agent org** — never a
production pool, never the price-guard's iad-ci pool (that one is warden's
internal write path, `spot-price-guard.md`). The motion is deliberately
minimal and symmetric:

```bash
# 1. Learn the live window; pick X inside it, ≠ current size, and small:
wardenctl pools                       # note lowerBound/upperBound of the pool
# 2. Scale one step inside the window (window-guarded client-side):
wardenctl scale <pool> <X>
# 3. Observe the effect (count changed, audit line written for your fingerprint):
wardenctl pools
# 4. Restore the original count:
wardenctl scale <pool> <original>
```

Every property to assert: step 2 returns `{"allowed": true, …}`; step 3 shows
the new count (allow for Spot fulfilment lag — re-list, don't assume); step 4
returns allowed and the pool is back to its original window position; warden's
audit log carries one allow line per applied scale with your fingerprint and
no token material anywhere. A 403 at any step is warden enforcing the
envelope — the exercise pool is mis-chosen, not warden broken; pick a pool
inside the class allowlist and bid cap and re-run. Never widen the envelope
to make an exercise pass.

## Rotation

Server-side, rotation is the overlap procedure in
`caller-token-provisioning.md` ("Rotation") — nothing here replaces it. The
consumer-side additions:

- The consumer's **new** value is piped into **its own store** at the same
  protected moment the overlap allowlist is written (the subshell in the
  rollout step 3 is the rotation step 2 with different version numbers).
- The consumer picks the new value up on its **next process start** (envFrom
  → env at pod start). A controller that re-reads env per call doesn't exist
  here by design — the same "read once" rule warden itself follows; restart
  the consumer through GitOps as part of the rotation.
- Order: consumer's new value in its store **and** overlap allowlist live
  (warden restarted) **before** the trim version removes the consumer's old
  token. If the consumer can't use its new token yet, stop at the overlap
  state — both its tokens work — and fix the consumer before trimming. Do
  not trim while a consumer is broken.
- Consumer-observed confirmation is `wardenctl auth-check` with the **new**
  fingerprint, after both restarts.

## Revocation

A consumer loses access by **absence on both sides**:

1. Remove its entry from warden's allowlist (re-store the field without it,
   `-cas` bump) — for a single-consumer leak, "rotation minus the overlap" in
   the runbook. Wait for `SecretSynced=True`, restart warden: its token is
   dead the moment that pod comes up. The running pod is the last place a
   revoked token works — the restart is load-bearing, not cleanup.
2. Write a **fresh value into the consumer's own store path** (it must not
   hold a working credential for a revoked identity) and roll the consumer,
   or decommission the path's references entirely if the consumer is gone.
   Agents cannot delete paths; superseding the value is the agent-side
   action, and removing the path itself is an operator OIDC action.
3. Verify by property: `wardenctl auth-check` from that consumer's context
   must now fail 401 with the old credential, and warden's audit shows
   `unauthorized` rejections carrying no fingerprint (rejected bearers log
   without identity by design).

## Rollback

- **A consumer that received a bad/typo'd value:** re-run the pipe from the
  protected context with the correct value; the previous version of its own
  store path is its undo (version history, never delete).
- **A rollout that broke the consumer** (new token not working yet): hold at
  the overlap state — both tokens valid — and fix forward. This is exactly
  the runbook's "Rolling back a consumer" rule: do not trim while a consumer
  is broken.
- **Warden-side revert of the allowlist:** the previous field content is in
  OpenBao version history; the restore-by-pipe procedure is in
  `caller-token-provisioning.md` ("Rotation" → rollback). Pair it with the
  same restart discipline.
- **The consumer deployment itself:** ordinary GitOps revert of its
  manifests (ArgoCD syncs the previous revision); the store path is untouched
  by a deploy revert, which is why credential stores are never the rollback
  mechanism for code.

## Why this shape

- **A distinct token per consumer, not the shared allowlist entry.** The
  allowlist field already supports comma-separated entries precisely so a
  second caller can be added without touching manifests
  (`caller-token-provisioning.md`, "Why this shape"). Per-consumer tokens
  make audit attribution exact (fingerprints differ), rotation independent
  (overlap/trim touches one entry), and revocation surgical (absence of one
  entry).
- **The consumer's token lives in the consumer's store, not warden's.**
  Warden's path is the *allowlist* — the server's own config. A consumer
  that can only read its own path can never enumerate the fleet's callers.
- **The window guard is client-side and the envelope is server-side.** Two
  independent bounds: the consumer refuses what it can predict warden would
  deny (cheap, no audit noise), and warden still decides authoritatively
  against live state on every request. Neither is a substitute for the
  other; `ScaleWithinWindow` failing open (list error) surfaces the error
  rather than sending an unguarded write.
- **`CheckAuth` demands the 401, not just the 200.** An auth check that only
  verifies the authenticated half passes against a server with auth
  disabled. The pair, in one call, is the cheapest continuously-runnable
  proof that the boundary both exists and admits this consumer.

## Traps

1. **Staging the credential first.** The order in Rollout is not stylistic:
  a token in a store with no consumer to use it is pure exposure surface.
  Code and deployment reference, then credential.
2. **Trimming before the consumer restarted.** The trim removes the old
  token; if the consumer is still running its previous process, it holds the
  old env and its calls start failing 401 mid-flight. Overlap live →
  consumer rolled → trim.
3. **Restarting warden before its Secret synced.** The pod re-reads the old
  allowlist and the change silently didn't happen
  (`caller-token-provisioning.md` trap 3 — same shape here from the
  consumer's side of the fence).
4. **Exercising against the wrong pool.** The exercise writes twice; against
  a production pool or the guarded iad-ci pool it is at best noise and at
  worst an outage drill. Designated worker pool only.
5. **Logging the token "just this once" for debugging.** Every failure path
  in `internal/intent` was built and tested to never carry it
  (`TestNoTokenMaterialAnywhere`). Debug with the fingerprint; if that is
  not enough, the answer is better errors, not credential exposure.
