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
it out-of-band and retry. Scale-to-zero on an autoscaled pool therefore requires
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
wire details in `docs/research/rackspace-spot-api.md`):** layer 2 is **real**,
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
  (`strconv.ParseUint` failure), not 409/422 — the 400/422 fallback below
  should never fire on this API, and a malformed RV fails closed through the
  generic error path instead (502 to the caller, nothing applied). warden only
  ever echoes RVs the API issued; `TestScaleNodePoolServerErrorStaysGeneric`
  pins that classification.

The 400/422 fallback stays as defense for a future upstream that rejects the
RV-carrying patch shape outright (warden then retries once without the
precondition and the ceiling rests on layer 1 alone, the topology-correct
guarantee).

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
