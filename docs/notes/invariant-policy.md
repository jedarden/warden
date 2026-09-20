# Invariant policy

The rules `internal/policy` enforces on every scale request. They are expressed
as **org-wide invariants**, not per-pool point checks — a per-pool "max N" is
trivially bypassed by adding a second pool.

## The envelope

| Invariant | Default | Enforced how |
|-----------|---------|--------------|
| Max total nodes (org ceiling) | `10` | `count + Σ UpperBound(other pools) ≤ max` |
| Allowed server classes | `gp.vs1.medium-iad` | target pool's `serverClass` must be in the set |
| Bid cap | `0.01` | refuse to grow a pool whose `bidPrice > cap` |
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

**Verified live (2026-09-16, credentialed probe — bead `warden-efd77a40`;
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

**Verified ≠ deployed:** the image currently running (`warden:0.1.0`, built
before the fix) contains neither layer. Shipping the fixed binary is a
separate rollout (declarative-config + new tag), deliberately not part of the
verification.

Any input warden cannot fully evaluate is denied:

- Unparseable `bidPrice` on the target pool ⇒ deny.
- Pool not found ⇒ deny (404).
- Unknown route / method ⇒ 404 (no default-allow).

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
- **Audit:** exactly one `audit` entry, `allowed=false`, carrying the same
  reason and the requested count — recorded at the moment of the decision,
  with no accompanying allows (the decision is made once; no patch means no
  re-decisions).
- **Retry:** none, automatically. A retry would re-list the same malformed
  snapshot and re-deny — pure load. This differs from the 409 path, where
  retrying is the whole point: there the state is *fresh*, here it is
  *broken*. The condition is upstream's to fix out-of-band (the same class of
  re-shape as lowering a floor); after the fix the identical request
  succeeds. Callers should treat the 403 like any other policy denial: do not
  retry as-is, fix the named pool, re-request.

Evaluation order: request `count ≥ 0` → snapshot bounds interpretable →
target floor → class allowlist → target bid → org total. A caller's own
mistake (negative count) is reported before upstream's, and the malformed
check runs before every pool-derived check because the floor, allowlist, bid,
and cap decisions all assume the snapshot means what it says.

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
