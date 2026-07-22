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
