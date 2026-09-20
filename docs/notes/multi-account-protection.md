# Multiple Spot accounts and grandfathered bids

Warden can serve an operator-declared set of Spot organizations across multiple
account credentials. The single-organization configuration remains supported:
`WARDEN_ORG_NAMESPACE` and `WARDEN_SPOT_REFRESH_TOKEN` retain the existing
`/v1/pools` and `/v1/pools/{name}/scale` API.

## Configure targets

Set `WARDEN_TARGETS_JSON` to a JSON array of targets. It contains **references
to environment variables**, never credential values. Each account alias uses
one refresh-token variable; several organizations may share that account.
Organization namespaces must be unique across all targets. The names and
namespaces in a request must exactly match the configured pair; Warden never
forwards an arbitrary namespace supplied by a caller.

```json
[
  {
    "account": "operations",
    "namespace": "org-example-operations",
    "refreshTokenEnv": "WARDEN_SPOT_REFRESH_TOKEN_OPERATIONS"
  },
  {
    "account": "workers",
    "namespace": "org-example-workers",
    "refreshTokenEnv": "WARDEN_SPOT_REFRESH_TOKEN_WORKERS",
    "allowScale": true,
    "maxTotalNodes": 10,
    "allowedServerClasses": ["ch.vs1.large-ord"],
    "maxBidPrice": 0.01
  }
]
```

`allowScale` defaults to **false** for every target. A scaling target must
declare its own node ceiling, class allowlist, and bid cap. These limits are
evaluated against all pools in that **one organization**; they are not pooled
across accounts or organizations. The named token variables are supplied by
Kubernetes Secrets sourced from the owning OpenBao instance. Warden reads each
value at startup and never returns or logs it. The operator must configure
those secret references and their OpenBao paths; adding an account alias does
not create credentials or grant Spot access.

`WARDEN_TARGETS_JSON` selects multi-target mode even with one entry. In that
mode, the legacy unscoped routes return `409` so a caller cannot accidentally
operate on an implicit default organization. The new authenticated routes are:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/accounts` | List configured account/organization pairs and whether scaling is enabled |
| `GET` | `/v1/accounts/{account}/organizations/{namespace}/pools` | List pools in one configured organization |
| `POST` | `/v1/accounts/{account}/organizations/{namespace}/pools/{name}/scale` | Scale one existing pool when that organization permits scaling |

Unknown account/organization pairs return `404`; a configured read-only target
returns `403` on scale. All `/v1` routes require a Warden caller token.

## Preserve grandfathered bids

Before **every** scale decision, Warden reads the target pool and its
`ServerClass.spec.minBidPricePerHour` from Spot. This is the current minimum
for a **new bid**, not the live market clearing price. If the pool's existing
`spec.bidPrice` is lower, Warden rejects **all** scale requests for that pool,
including no-op, growth, shrink, and zero. The pool and its old bid remain
untouched through Warden. Missing or invalid prices and failed class reads
also block the write.

An allowed scale carries the pool's `metadata.resourceVersion` as an upstream
write precondition. If the revision is missing, changes concurrently, or the
upstream rejects the precondition, Warden does not send an unguarded fallback
patch. On a conflict it re-reads the pool and class minimum, then re-decides.

This protection covers Warden's API. Spot console actions, Terraform, and
other holders of an organization credential can still modify a pool directly.
Spot market preemption can still terminate nodes when the market price exceeds
their bid. Keep direct Spot write credentials away from agents that should use
Warden, and apply corresponding checks to other authorized write paths.
