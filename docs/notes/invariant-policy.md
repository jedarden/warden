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
`desiredCount`. Summing upper bounds (not current sizes) makes the cap a true
ceiling on how many nodes the org can *ever* hold.

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

## Fail-closed

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
