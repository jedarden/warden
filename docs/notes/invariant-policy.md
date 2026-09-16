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

**Verified status of layer 2:** whether the Spot ngpc API exposes
`resourceVersion` and honors it in merge patches is *not verified live* — no
credentialed probe was possible from the dev box (2026-09-16), and the recorded
live-response samples do not include it. Therefore:

- If the upstream ignores the precondition, it is harmless dead weight; layer 1
  still enforces the ceiling.
- If the upstream *rejects* an RV-carrying patch shape outright (400/422),
  warden logs a warning and retries the identical patch once without the
  precondition rather than failing every scale. The ceiling then rests on
  layer 1 alone, which is the topology-correct guarantee.

`TestConcurrentScaleNeverExceedsOrgCeiling` pins this: its fake upstream
applies patches unconditionally (deliberately no server-side CAS — the
worst-case upstream), and proves 20 concurrent individually-allowed requests
never push the summed upper bounds past the cap, at any applied state or in
the final state. Removing the single-flight mutex makes that test fail.

Follow-up: verify against the live API whether list responses carry
`resourceVersion` and whether patches honoring it return 409 on mismatch. If
they never do, delete layer 2 and this note's conditional language.

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
