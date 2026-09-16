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

## Spec fields (VERIFIED 2026-08-02)

- `serverClass` — instance type, e.g. `gp.vs1.medium-iad` (region-suffixed).
- `bidPrice` — max spot price.
- `desired` — fixed node count (fixed-size pools) — **NOT** `desiredCount`.
- `autoscaling.enabled` / `autoscaling.minNodes` / `autoscaling.maxNodes` —
  cluster-autoscaler bounds (camelCase, NOT snake_case). When autoscaling is
  enabled, do **not** set a fixed desired count (Terraform: `desired_server_count`
  must be omitted).
- `cloudSpace` — cloudspace ref (capital S) — **NOT** `cloudspace`.
- No `region` on the pool (region lives on the cloudspace / serverclass).

> The docs don't publish the exhaustive spec, and Terraform uses snake_case
> (`server_class`, `bid_price`, `min_nodes`) which differs from the CRD's
> camelCase. All JSON field names have been verified against live API responses
> and official Spot Go SDK documentation. warden isolates the correct schema in
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
- Autoscaling fields use **camelCase**: `minNodes`, `maxNodes` (VERIFIED 2026-08-02
  against official Spot Go SDK documentation).
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

## resourceVersion & optimistic concurrency — VERIFIED against the live API (2026-09-16)

Probed with the `apexalgo-agent` org refresh token against pool
`88c5e399-70ac-4825-a40f-7348395daf52` (namespace `org-knyiltp8zznvkz5g`,
`agent-sandbox`, `ch.vs1.large-ord`, `desired: 2`, autoscaling disabled).
This closes the open question from the 2026-07-27 probe (whose recorded
samples predated metadata capture) — see `docs/notes/invariant-policy.md`
"Concurrency". All patches were merge patches
(`application/merge-patch+json`), the shape warden sends.

- **RV is exposed.** `metadata` on GET **and** on LIST items carries
  `resourceVersion` — a decimal string, e.g. `"77772631"` — alongside
  `generation`, `uid`, `managedFields`, `finalizers`, `ownerReferences`:
  apiserver-shaped metadata throughout.
- **Current RV → 200.** A patch whose `metadata.resourceVersion` matches
  current state applies normally.
- **Mismatched RV → 409, write rejected.** Same patch with a wrong RV:
  `409` and the Kubernetes-standard conflict body:
  `Operation cannot be fulfilled on spotnodepools.ngpc.rxt.io "<name>": the
  object has been modified; please apply your changes to the latest version
  and try again`.
- **The precondition protects the write, not just the patch shape.** A patch
  carrying a *state change* (desired 2→3) plus a stale RV returned 409 and
  `desired` stayed 2 — nothing was applied.
- **No-op patches do not bump the RV.** Writing the current value back left
  `resourceVersion` unchanged; the RV advances only on real modification.
- **RV is parsed as uint64 server-side.** A non-numeric RV (`"bogus-rv-xyz"`)
  yields **500** `strconv.ParseUint: parsing "bogus-rv-xyz": invalid syntax` —
  not 409/422. Consequence for warden: only echo RVs the API issued; a
  malformed RV fails closed via the generic error path, and the
  precondition-shape fallback (400/422) never fires on this API.

Probe safety note: every probe patch wrote the pool's then-current count back
(no-op), so an inert precondition could not have mutated the org; the one
state-changing payload was guarded by an already-verified stale RV and was
confirmed unapplied.
