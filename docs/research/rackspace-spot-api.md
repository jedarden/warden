# Rackspace Spot public API — reference notes

Source: Rackspace Spot docs, fetched 2026-07-22.
- https://spot.rackspace.com/docs/rackspace-spot-public-api
- https://spot.rackspace.com/docs/en/autoscaling-a-spot-node-pool
- https://github.com/rackerlabs/terraform-provider-spot

## API shape

- **Kubernetes-style REST API** using CRDs (resource types, namespaces, standard
  verbs GET/POST/PATCH/DELETE).
- **Base URL:** `https://spot.rackspace.com`
- **Auth:** OAuth 2.0 Bearer token in `Authorization: Bearer <access_token>`.

## Obtaining a token

Exchange a **refresh token** (from the Spot Console → *API Access → Terraform*)
at the token endpoint:

```
POST https://login.spot.rackspace.com/oauth/token
Content-Type: application/x-www-form-urlencoded

grant_type=refresh_token
client_id=mwG3lUMV8KyeMqHe4fJ5Bb3nM1vBvRNa
refresh_token=<your_refresh_token>
```

Response includes `access_token` and `expires_in`.

## Org namespace

Namespaces are `org-<id>`. Discover via:

```
GET /apis/auth.ngpc.rxt.io/v1/organizations
```

## SpotNodePool resource

- **API group / version:** `ngpc.rxt.io/v1`
- **Kind:** `SpotNodePool`
- **Endpoints:**
  - List:   `GET    /apis/ngpc.rxt.io/v1/namespaces/{ns}/spotnodepools`
  - Create: `POST   /apis/ngpc.rxt.io/v1/namespaces/{ns}/spotnodepools`
  - Read:   `GET    /apis/ngpc.rxt.io/v1/namespaces/{ns}/spotnodepools/{name}`
  - Modify: `PATCH  /apis/ngpc.rxt.io/v1/namespaces/{ns}/spotnodepools/{name}`
  - Delete: `DELETE /apis/ngpc.rxt.io/v1/namespaces/{ns}/spotnodepools/{name}`

## Spec fields (from docs; VERIFY against live object)

- `serverClass` — instance type, e.g. `gp.vs1.medium-iad` (region-suffixed).
- `bidPrice` — max spot price.
- `desiredCount` — fixed node count (fixed-size pools).
- `autoscaling.enabled` / `autoscaling.minNodes` / `autoscaling.maxNodes` —
  cluster-autoscaler bounds. When autoscaling is enabled, do **not** set a fixed
  desired count (Terraform: `desired_server_count` must be omitted).
- `cloudspace`, `region`.

> The docs don't publish the exhaustive spec, and Terraform uses snake_case
> (`server_class`, `bid_price`, `min_nodes`) which may differ from the CRD's
> camelCase. Confirm the exact JSON field names with a live
> `GET spotnodepools/<name>` before go-live. warden isolates this in
> `internal/spot/types.go`.

## Notes

- Cloudspace *creation* is currently UI/Terraform only ("API-based creation
  supported in future"). warden does not create cloudspaces or pools regardless —
  it only scales existing pools.
- Underlying scaling is the upstream Kubernetes Cluster Autoscaler.

## VERIFIED against the live API (2026-07-27)

Probed with the `apexalgo-agent` org refresh token. Corrections to the doc-sourced
assumptions above:

### Auth — bearer is the `id_token`, not `access_token`
The OAuth refresh-token exchange returns both. The Spot API validates the OIDC
**`id_token`** (a JWT); sending `access_token` (opaque) yields
`"Jwt is not in the form of Header.Payload.Signature"` (401). Use `id_token`.
Response also carries `expires_in: 86400` (24h) and `scope: openid profile email
offline_access`. The id_token claims include `org_id`, `group`
(e.g. `cloudspace-admin`), and `aud` (the client_id).

### Namespaces
`GET /apis/auth.ngpc.rxt.io/v1/organizations` → `{organizations:[{id,name,
metadata:{namespace}}]}`. The namespace is the org id lowercased with `_`→`-`
(e.g. `org_kNYILtp8ZZNvkZ5g` → `org-knyiltp8zznvkz5g`). The org-listing endpoint
enumerates every org the underlying *user* belongs to, but resource RBAC is
per-org: this token **403s** on other orgs' namespaces.

### SpotNodePool spec — real field names
```json
{"serverClass":"ch.vs1.large-ord","bidPrice":"0.01","cloudSpace":"agent-sandbox",
 "desired":1,"autoscaling":{"enabled":false},
 "customAnnotations":{},"customLabels":{},"customTaints":[]}
```
- Fixed count is **`desired`** (NOT `desiredCount`).
- Cloudspace ref is **`cloudSpace`** (capital S).
- No `region` on the pool (region lives on the cloudspace / serverclass).

### ServerClass — pricing & the minimum bid
`GET /apis/ngpc.rxt.io/v1/serverclasses` is **cluster-scoped**, returns ~110
classes in one call. Per class:
- `spec.minBidPricePerHour` — **the minimum bid (the floor)**. Distinct from market
  price. `gp.vs1.medium-iad` = **`0.01`** (was `0.001` — the floor moved 10×).
- `status.spotPricing.marketPricePerHour` / `hammerPricePerHour` — live market /
  billed rate (`0.001` for medium-iad at probe time).
- `spec.onDemandPricing.{cost,interval}` — e.g. medium-iad `{cost:"0.019",interval:"1h"}`.
- `spec.resources` — medium = `{cpu:"2",memory:"3.75GB",disk:"100GB"}`.
- `status.{available,capacity,reserved,lastAuction}` — live capacity. medium was
  `available:0, capacity:0` in every region at probe time (nothing to bid on).
- Other `spec` keys: `availability, category, displayName, flavorType, provider, region`.

So the minimum bid needs **no probe / dummy bid** — it's a published field. One
authenticated `GET serverclasses` gives minBid + market + capacity for all classes.
