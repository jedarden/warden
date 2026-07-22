# Invariant policy

The rules `internal/policy` enforces on every scale request. They are expressed
as **org-wide invariants**, not per-pool point checks — a per-pool "max N" is
trivially bypassed by adding a second pool.

## The envelope

| Invariant | Default | Enforced how |
|-----------|---------|--------------|
| Max total nodes (org ceiling) | `10` | `count + Σ UpperBound(other pools) ≤ max` |
| Allowed server classes | `gp.vs1.medium-iad` | target pool's `serverClass` must be in the set |
| Bid cap | `0.001` | refuse to grow a pool whose `bidPrice > cap` |
| Node count | `≥ 0` | negative denied |

`UpperBound(pool)` = `autoscaling.maxNodes` if autoscaling is enabled, else
`desiredCount`. Summing upper bounds (not current sizes) makes the cap a true
ceiling on how many nodes the org can *ever* hold.

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
