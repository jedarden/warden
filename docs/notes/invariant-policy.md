# Invariant policy

The rules `internal/policy` enforces on every scale request. They are expressed
as **org-wide invariants**, not per-pool point checks — a per-pool "max N" is
trivially bypassed by adding a second pool.

## The envelope

| Invariant | Default | Enforced how |
|-----------|---------|--------------|
| Max total nodes (org ceiling) | `10` | growth must keep `count + Σ UpperBound(other pools) ≤ max`; an already-over-cap org permits only strict target reductions |
| Allowed server classes | `gp.vs1.medium-iad` | growth requires the target pool's `serverClass` to be in the set; an existing out-of-set pool permits only strict reduction |
| Bid cap | `0.01` | growth requires `bidPrice ≤ cap`; an existing over-cap or unparseable bid permits only strict reduction |
| Node count | `≥ 0` | negative denied |
| Autoscaling window | — | autoscaled target: `count ≥ autoscaling.minNodes` |

`UpperBound(pool)` = `autoscaling.maxNodes` if autoscaling is enabled, else
`spec.desired` (the live field name is `desired`, not `desiredCount` — see
`docs/research/rackspace-spot-api.md`). Summing upper bounds (not current sizes)
makes the cap a true ceiling on how many nodes the org can *ever* hold.

## Scale semantics — fixed vs autoscaled pools

The intent API has one operation: set a node count. What the count *is* depends
on the kind of pool (decided 2026-09-16, bead `warden-16be4ebe`):

- **Fixed pool** — the count is the size: warden patches `spec.desired`.
- **Autoscaled pool** — the count is the **ceiling**: warden patches
  `spec.autoscaling.maxNodes`. It never writes `desired` on an autoscaled pool:
  the upstream cluster-autoscaler owns that field, and a fixed desired there is
  documented invalid (`docs/research/rackspace-spot-api.md`) — it would be
  fought and reverted. Ceiling semantics also match the cap accounting exactly:
  a scale request sets the very quantity (`UpperBound`) the org total sums.

warden never writes `autoscaling.minNodes`. A count below the pool's current
`minNodes` would leave an inverted window (`maxNodes < minNodes`), so it is
**denied** — fail closed. Lowering a floor is a pool re-shape, not a scale; do
it out-of-band and retry. Since bead `warden-db1e5d95` the enforced floor is
visible to callers: `GET /v1/pools` returns each pool's `lowerBound`
(`minNodes` when autoscaled, `0` on a fixed pool) next to `upperBound`, so a
caller can stay inside the window without probing for the 403.
Scale-to-zero on an autoscaled pool therefore requires
`minNodes = 0` and means `maxNodes = 0`: the autoscaler may not add nodes (the
pool is paused), not that anything is deleted.

## Out-of-policy existing pools — strict monotonic remediation

This is the exception to the ordinary envelope checks (decided 2026-10-03,
bead `warden-5219c29f`). Warden cannot create/delete a pool or change its class,
bid, or autoscaling floor; for an existing pool already outside the class
allowlist, carrying an over-cap or unparseable configured bid, or belonging to
an organization whose current summed upper bounds exceed the ceiling, the only
safe operation it can express is winding that target down.

The rule is strict and compares the requested count with the target's current
`UpperBound`:

| Requested count | Decision | Result |
|---|---|---|
| `count < UpperBound` | allow | strict remediation; patch only the fixed `desired` or autoscaled `maxNodes` count, with the snapshot `resourceVersion` |
| `count == UpperBound` | deny | no-op is not remediation; no patch is sent |
| `count > UpperBound` | deny | growth cannot deepen or preserve the drift; no patch is sent |

Scale-to-zero is therefore allowed for a fixed out-of-policy pool when its
current bound is positive. It is allowed for an autoscaled pool only when
`minNodes = 0`; a nonzero floor still denies zero or any count below that floor.
An org reduction may remain above the ceiling — it is still allowed because the
target bound strictly fell. The caller receives `200` with an allow reason of
the form `strict out-of-policy reduction permitted: <drift>; org ceiling
<requested-total>/<cap>`. A no-op receives `403` with `out-of-policy pool
requires a strict reduction`, and growth receives `403` with either
`refusing to grow out-of-policy pool` (class/bid drift) or the ordinary
`request would bring org node ceiling...` reason. Every allowed request still
uses a count-only merge patch and the resource-version precondition; denials
send zero patches.

This monotonic exception does not relax interpretation or ownership rules:
malformed node-count state, a count below `minNodes`, a missing bid, a failed
server-class lookup, and the separate `GrandfatheredBid` rule for a valid bid
below the current new-bid minimum remain denied in every direction. An
unparseable non-empty configured bid is handled by this strict-reduction rule:
shrink is allowed, while hold and growth are denied.

## Concurrency — the ceiling cannot be raced (decided 2026-09-16, bead `warden-d76280fb`)

The ceiling is evaluated read-before-write: list every pool, sum upper bounds,
then patch. Taken naively that is a TOCTOU window — two concurrent scale
requests could both pass against the same snapshot and both apply, leaving the
summed upper bounds above `MAX_TOTAL_NODES`, the one guarantee warden exists to
enforce. The window is closed by two layers:

1. **Single-flight (primary mechanism).** The whole read→evaluate→write
   sequence in `scalePool` runs under one per-instance mutex. One mutex for all
   pools, not per-pool: the invariant is org-wide, so even scales of *different*
   pools race each other. This alone is sufficient for the deployed topology —
   the Deployment runs `replicas: 1` and warden holds the org's only Spot
   credential, so every write to the org flows through that mutex.

2. **`resourceVersion` precondition (defense in depth).** The outbound patch
   echoes the snapshot's `metadata.resourceVersion`. On APIs with Kubernetes
   semantics this is an optimistic-concurrency check: a mismatch is rejected
   with 409, meaning the state the decision was made against is stale. On 409
   warden re-lists, re-evaluates against the fresh state (the request may
   now exceed the cap and is then denied), and retries — one planned attempt
   plus up to three re-decisions (`maxScaleAttempts`). If every attempt loses
   the race, warden fails closed: 409 to the caller, nothing applied, audited
   as a deny.

**Verification status — settled; all three parent checks confirmed live.**
The parent verification bead `warden-efd77a40` was answered by a credentialed
probe on 2026-09-16 (below), and every check was independently re-run fresh on
2026-11-08. The precondition is verified, not assumed — nothing in this
document records it as unverified:

- **(a) `metadata.resourceVersion` exposure — verified 2026-09-16 and again
  2026-11-08** (fresh field-presence probe, bead `warden-c5680dda`; below):
  present on GET and on the LIST warden snapshots, decimal JSON string.
- **(b) stale-resourceVersion merge-patch rejection — verified 2026-09-16 and
  again 2026-11-08** (fresh stale-RV patch probe, bead `warden-fc7bbe53`;
  below): a merge patch carrying rv−1 is rejected with **409** and the write
  is **not applied**; the identical body with the current RV applies.
- **(c) the warden retry path — verified live end-to-end 2026-11-08**
  (conflict-injection probe, bead `warden-06e84fb5`; below): 409 → re-list →
  re-evaluate → retry succeeds; exhausting `maxScaleAttempts` → 409 to the
  caller, audited deny, nothing applied.

**Verdict: ceiling-race layer 2 is real, not inert.** The live ngpc API
enforces the `resourceVersion` precondition, so the inert clause the parent
bead asked to be recorded conditionally does **not** apply. For the record, it
would have read: *if upstream silently ignores the precondition, ceiling-race
layer 2 is inert and the single-flight mutex (layer 1) is the only effective
defense* — that is not how the upstream behaves, as (b) shows. No open
question about layer 2 remains in this document. Layer 1 stays the primary
mechanism regardless, because it does not depend on upstream semantics at all.

**Original live probe (2026-09-16, credentialed — bead `warden-efd77a40`;
independently re-run on close the same day with strictly no-op-content
patches, org state byte-identical pre/post; wire details in
`docs/research/rackspace-spot-api.md`):** layer 2 is **real**,
not dead weight. The ngpc API behaves like an apiserver exactly where the
design needs it to:

- SpotNodePool objects expose `metadata.resourceVersion` on GET **and** on the
  LIST warden snapshots (a decimal string, e.g. `"77772631"`).
- A merge patch carrying the *current* RV applies (200). One carrying a
  *mismatched* RV is rejected with 409 and the Kubernetes conflict message
  ("Operation cannot be fulfilled … the object has been modified").
- Verified with a **state-changing** patch (a count bump) carrying a stale RV:
  409, and the count change was *not* applied — the precondition rejects the
  write, it does not apply-and-conflict.
- A no-op patch does not bump the RV; only real modifications do.
- Wrinkle: ngpc parses the RV as uint64, so a non-numeric RV gets HTTP **500**
  (`strconv.ParseUint` failure), not 409/422. A malformed RV fails closed through the
  generic error path instead (502 to the caller, nothing applied). warden only
  ever echoes RVs the API issued; `TestScaleNodePoolServerErrorStaysGeneric`
  pins that classification.

Warden refuses to patch when the snapshot lacks a resourceVersion. If an
upstream rejects an RV-carrying patch with 400/422, Warden returns 502 without
an unconditional retry. This also protects a grandfathered bid from a pool
change between the policy read and the attempted write.

**Fresh field-presence probe (2026-11-08T18:43:04Z–18:43:06Z, bead
`warden-c5680dda`):** using the
OpenBao reference `secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization`
(`api-token`) to authenticate read-only requests, LIST returned HTTP 200 with
one item and the field-level sample
`items[0].metadata.resourceVersion = "84339248"` (JSON string); GET for that
same pool returned HTTP 200 with
`metadata.resourceVersion = "84339248"` (JSON string). No full response was
recorded and no patch was sent. Therefore parent bead item **(a) holds**:
live SpotNodePool objects expose `metadata.resourceVersion`.

**Fresh stale-resourceVersion patch probe (2026-11-08T18:59:17Z–18:59:23Z,
bead `warden-fc7bbe53`):** same credential reference as above (OpenBao
`secret/rs-manager/warden/rackspace-spot/apexalgo-agent-organization`, field
`api-token`; token exchanged for the bearer `id_token` and staged only in a
mode-600 tmpfs file consumed via `curl -H @file` — never argv, never
printed). Same live pool (`88c5e399-70ac-4825-a40f-7348395daf52`, pre-state
`rv="84339248"`, `generation=2`, `spec.desired=2`, `autoscaling.enabled=false`).
Patch shape exactly as warden sends it (`application/merge-patch+json`,
`{"metadata":{"resourceVersion":…},"spec":{…}}`), with a fresh GET
immediately before each send so the RVs were exact at send time:

- **P1 — stale RV, no-op content.** Patch echoed `"84339247"` (rv−1) while
  current was `"84339248"`: **HTTP 409**, body
  `Operation cannot be fulfilled on spotnodepools.ngpc.rxt.io "<pool>": the
  object has been modified; please apply your changes to the latest version
  and try again`. Immediately following GET: `rv` still `"84339248"`,
  `generation` still `2`, `spec.desired` still `2` — the write was
  **rejected, not applied**.
- **P2 — control, current RV, identical no-op content.** Same patch body
  with `"84339248"`: **HTTP 200**. The 409 in P1 is therefore attributable
  to the stale RV alone, not to the content or patch shape.
- Both patches wrote the pool's then-current count back (strict no-op
  content), and rv/generation/desired were identical pre- and post-probe
  (`84339248` / `2` / `2`) — the org was not mutated, consistent with the
  known "no-op patches do not bump the RV" behavior.

Therefore parent bead item **(b) holds**: the live ngpc API **enforces** the
`resourceVersion` precondition on merge patches — a stale RV is rejected
with 409. Ceiling-race layer 2 is not inert.

**Live end-to-end retry-path verification (2026-11-08T19:15:27Z–19:16:28Z,
bead `warden-06e84fb5`):** this time the loop itself, not just its inputs,
ran against the live API. A warden binary built from committed `main` HEAD
ran with the deployed rs-manager policy (`ch.vs1.large-ord` allowlist, cap
10, bid cap 0.01; spot guard not started), pointed at a local
conflict-injection proxy that forwards every request to the real ngpc API
verbatim and rewrites each patch's `metadata.resourceVersion` to
(sent − 1) — the exact value class the probe above proved the real API
rejects with 409. The proxy fabricates no response: every status below is
the real upstream's. Patch content was strictly no-op (`desired 2→2`), and
the target was the same pool (rv `84339248`, generation `2`, autoscaling
disabled; bid `0.01` meets the class minimum `0.01`, so the request is a
plain in-policy allow).

- **Conflict → re-list → re-evaluate → retry succeeds.** A `{"count":2}`
  request with only the first patch rewritten. Wire: LIST `200` → class GET
  `200` → PATCH carrying `84339248`, sent as `84339247`, **409** (the real
  conflict message) → warden logged `scale: pool changed concurrently,
  re-evaluating against fresh state` (attempt 1) → **LIST `200`** → class
  GET `200` → PATCH carrying `84339248` (fresh snapshot RV, passed through
  unmodified) → **200**. The caller got `200` with `allowed:true` and
  `scale "88c5e399-…" to 2 (org ceiling 2/10)`; warden audited one allow per
  decision (two entries) — the documented loop end to end.
- **Exhaustion → 409 to the caller, audited deny, nothing applied.** The
  identical request with every patch rewritten: exactly `maxScaleAttempts`
  (4) patches, each sent as `84339247`, each **409** from the real API, a
  LIST before each attempt, re-evaluation warnings for attempts 1–3, and an
  audit trail of **four allows (one per re-decision) plus one final deny**
  (`pool changed concurrently on every attempt; request not applied`). The
  caller received **409** with `pool changed concurrently on every attempt;
  retry later`. A post-probe LIST showed rv/generation/desired
  byte-identical to pre-probe (`84339248`/`2`/`2`) — no write ever landed
  without a fresh-snapshot decision behind it.

Parent bead item **(c) holds**: warden's re-list/re-evaluate/retry loop and
its fail-closed exhaustion behave exactly as this document describes them,
observed live end to end.

Method note: the conflict was injected deterministically by the proxy's RV
rewrite rather than by a real concurrent writer — the only way to bump the
live RV is a real modification, and deliberately mutating a production
pool, even transiently, is not an authorized probe action. From warden's
side the two are indistinguishable: it held a snapshot the API declared
stale and reacted exactly as documented. Because nothing upstream actually
changed, the fresh snapshot equals the stale one, so the branch where
re-evaluation *flips* the decision to a deny on fresh state is not
live-exercised; it stays pinned by
`TestRetryReDecisionDeniesSnapshotThatBecameMalformed`. The implemented
loop (`internal/server/server.go`, `maxScaleAttempts = 4`) matched this
document exactly — one planned attempt plus three re-decisions, one audit
entry per decision, fail-closed 409 + audited deny on exhaustion; no
mismatch found. Injector trace excerpt:
`docs/research/rackspace-spot-api.md`.

`TestConcurrentScaleNeverExceedsOrgCeiling` pins layer 1: its fake upstream
applies patches unconditionally (deliberately no server-side CAS — the
worst-case upstream), and proves 20 concurrent individually-allowed requests
never push the summed upper bounds past the cap, at any applied state or in
the final state. Removing the single-flight mutex makes that test fail. Layer
2's retry path is pinned by `TestScaleRetriesAfterConflictWithFreshSnapshot`,
`TestScaleConflictExhaustedFailsClosed`, and
`TestScaleConflictExhaustionAuditsDeny` (409 to the caller on exhaustion,
with a final audited deny); the wire sequence they encode — 409 on a stale RV,
then success on a fresh-RV patch — is what the live probe reproduced against
the real API.

Both layers are deployed in `warden:0.2.0` as of 2026-09-22. The rollout used
`declarative-config` commit `abbd0485`; ArgoCD Application
`warden-ns-rs-manager` was Synced and Healthy, the pod reported Ready on the
published image digest, and authenticated list plus policy-denial checks passed.

Other input handling remains fail-closed:

- An unparseable non-empty `bidPrice` on the target pool ⇒ strict reduction
  only; hold and growth deny. A missing bid remains fail-closed before the
  envelope decision.
- Pool not found ⇒ deny (404).
- Unknown route / method ⇒ 404 (no default-allow).
- A pool-list body that fails to decode (e.g. a count field that is not an
  integer) ⇒ 502, no policy decision reached — see "Malformed upstream
  state".

## Malformed upstream state — fail closed (decided 2026-09-17, bead `warden-3694ee8d`)

The bid rule above covers one uninterpretable field. The *node-count* fields
had the same gap, and a worse failure direction: before this rule existed, a
pool with a malformed count state flowed into the org total through
`UpperBound()` as **0 or a negative number**, silently *shrinking* the summed
ceiling and letting a breach through — the one direction this policy must
never fail in. `NodePool.CountBoundError()` now enumerates the malformed
states, and `EvaluateScale` denies while **any pool in the snapshot** carries
one (the total is computed from all of them, so one broken pool poisons every
scale):

| Malformed state (any pool in the snapshot) | Why it fails closed |
|---|---|
| fixed pool with no `desired` | `UpperBound` reads 0 |
| fixed pool with `desired` < 0 | `UpperBound` subtracts |
| autoscaled pool with `minNodes` < 0 | the window is not interpretable state |
| autoscaled pool with `maxNodes` < 0 | `UpperBound` subtracts |
| autoscaled pool with `maxNodes` < `minNodes` | the autoscaler holds the pool at ≥ `minNodes`, so `maxNodes` under-counts a floor already above the "ceiling" |

### Decision matrix

Every row is a **deny** — the intent API has no hold or pending outcome;
each scale decision is a synchronous allow-or-deny. The five
`CountBoundError` conditions resolve identically at the server layer (see
"Resolution" below for the shared status, audit, and retry semantics); the
`reason` detail is the differentiator. The reason's outer form depends only
on *which* pool is broken: `target pool state is malformed: <detail> (fix
the pool out-of-band; warden will not act on state it cannot interpret)`
when it is the pool being scaled, else `org snapshot has malformed pool
state: pool "<name>": <detail> (fail closed)`. The last row is the
wire-invalid sibling — a count field that cannot decode at all never
reaches policy and fails closed one layer earlier, with different
semantics.

| Input condition (upstream wire state) | Pool kind | Decision | HTTP | Stable reason family and canonical `<detail>` | Audit event | Retry |
|---|---|---|---|---|---|---|
| `spec.desired` absent or `null` | fixed | deny | `403` JSON | `target pool state is malformed` or `org snapshot has malformed pool state`; `fixed pool has no desired count` | one `msg:"audit"`, `action:"scale"`, `allowed=false` | none automatic; repair out-of-band; zero patches |
| `spec.desired` < 0 | fixed | deny | `403` JSON | same families; `desired count %d is negative` | same event; requested target and count retained | none automatic; repair out-of-band; zero patches |
| `autoscaling.minNodes` < 0 | autoscaled | deny | `403` JSON | same families; `autoscaling minNodes %d is negative` | same event; requested target and count retained | none automatic; repair out-of-band; zero patches |
| `autoscaling.maxNodes` < 0 | autoscaled | deny | `403` JSON | same families; `autoscaling maxNodes %d is negative` | same event; requested target and count retained | none automatic; repair out-of-band; zero patches |
| `maxNodes` < `minNodes` (window inverted) | autoscaled | deny | `403` JSON | same families; `autoscaling maxNodes %d is below minNodes %d` | same event; requested target and count retained | none automatic; repair out-of-band; zero patches |
| any count field present but not an integer (`"desired": "three"`, `1.5`, `true`) | any | deny — at decode, before policy | `502` plain text `upstream error` | n/a — no policy reason | no `msg:"audit"` event; no decision | yes, later — upstream must decode again; zero patches |

**Intentional non-malformed cases**, recorded so the denials above cannot be
mistaken for over-broad validation:

- `minNodes = maxNodes = 0` on an autoscaled pool — the documented paused
  state, legal (see "Scale semantics").
- A vestigial `desired` on an autoscaled pool — not policed (below).
- An `autoscaling` block with `enabled: false` — the pool is judged as
  fixed; its `minNodes`/`maxNodes` are vestigial and never validated, only
  `desired` is. Symmetric with the vestigial-`desired` case.
- `desired: 0` on a fixed pool — a legal zero, distinct from absent/`null`
  (a JSON `null` decodes exactly like an absent field: both leave the
  pointer nil, both are the "no desired count" deny).

A negative `minNodes` is denied even when `maxNodes ≥ 0` would itself be a
sane ceiling: guessing at which half of an invalid window upstream means is
how a fail-open hole gets born. `maxNodes = minNodes = 0` stays legal — that
is the documented paused state ("Scale semantics" above). On the wire, absent
and zero are indistinguishable, so "absent bounds on an enabled autoscaler"
decodes as paused 0/0 and is fine, while "absent `desired` on a fixed pool"
is malformed by the same construction.

`desired` on an autoscaled pool is deliberately **not** validated: the
upstream cluster-autoscaler owns that field, warden never reads it there, and
a vestigial value — documented-invalid but harmless to the ceiling — is not
warden's to police. `UpperBound()` itself is unchanged (still reads 0 or the
negative through): policy refuses to sum a snapshot containing a malformed
pool rather than the method papering over it.

An inverted window on the **target** is denied even at a count that would
repair it (e.g. window `min 8 / max 2`, count 9): the same stance as the
`minNodes` rule — warden does not act on state it cannot interpret, and a
scale is not the tool for re-shaping a pool. Fix it out-of-band, then retry.

Every row above resolves identically at the server layer, because they are
all `EvaluateScale` denies:

- **Decision:** deny. The patch is never constructed or sent (zero upstream
  writes; the `maxScaleAttempts` conflict loop never starts, since that only
  runs after an allowed decision).
- **Status:** `403 Forbidden`, JSON `{"allowed": false, "reason": …}` — the
  same uniform policy-denial shape as every other envelope violation. The
  reason names the offending pool and field: `target pool state is malformed:
  …` when it is the pool being scaled, `org snapshot has malformed pool state:
  pool "<name>": …` when a bystander pool is the problem.
- **Audit:** exactly one stable `msg:"audit"` event, recorded at the moment of the
  decision, with no accompanying allows (the decision is made once; no patch
  means no re-decisions). Its payload is the complete `audit.Entry` shape and
  nothing else (pinned by `internal/audit`'s `documentedKeys`): `caller`
  (token fingerprint, never the raw token), `remote_addr`, `action="scale"`,
  `account`, `namespace`, `pool` — the pool the caller asked to scale, even
  when the reason names a different (bystander) pool as the offender —
  `count` (the requested count), `allowed=false`, and `reason` (the
  canonical reason above). The two outer malformed-state reason families and
  the detail labels in the matrix are stable names; only values interpolated
  into `%d`/`%q` positions vary. A decode failure has no event because it never
  reaches an authorization decision.
- **Retry:** none, automatically. A retry would re-list the same malformed
  snapshot and re-deny — pure load. This differs from the 409 path, where
  retrying is the whole point: there the state is *fresh*, here it is
  *broken*. The one place warden's own retry meets malformed state is the
  conflict loop: an out-of-band writer that breaks a pool between warden's
  patch and its re-decision makes that re-decision deny on the fresh
  snapshot — the caller then sees 403, not 409
  (`TestRetryReDecisionDeniesSnapshotThatBecameMalformed`). The condition is
  upstream's to fix out-of-band (the same class of
  re-shape as lowering a floor); after the fix the identical request
  succeeds. Callers should treat the 403 like any other policy denial: do not
  retry as-is, fix the named pool, re-request.

Evaluation order: request `count ≥ 0` → snapshot bounds interpretable →
target floor → class allowlist → target bid → org total. A caller's own
mistake (negative count) is reported before upstream's, and the malformed
check runs before every pool-derived check because the floor, allowlist, bid,
and cap decisions all assume the snapshot means what it says. One gate sits
in front of all of these at the server layer: the grandfathered-bid check
runs before `EvaluateScale` entirely, so a target failing both reports the
grandfather reason — either way a deny, never an allow.

## Impossible by construction

These are not on a deny-list — there is simply no way to express them through
the intent API, because it has no endpoint or field for them:

- create or delete a node pool
- create or delete a cloudspace
- change a pool's server class
- change a pool's bid price
- change a pool's autoscaling floor (`minNodes`) or toggle autoscaling on/off

warden always constructs the outbound patch itself and only ever sets the node
count on a pool that already exists.

## Escape hatches this closes

A naïve "cap total nodes" check that forwarded caller patches would still be
bypassable by: raising a pool's own max, adding a new pool, changing class, or
creating another cloudspace. warden closes all four by owning the patch and
exposing no pool/cloudspace lifecycle operations at all.

## Changing the envelope

`MAX_TOTAL_NODES`, `ALLOWED_SERVER_CLASSES`, `MAX_BID_PRICE` are config
(env from the Deployment). Changing them is a `declarative-config` commit +
ArgoCD sync — deliberately a reviewed, GitOps change, not a runtime toggle.
