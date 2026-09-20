# warden

Warden also supports explicit routing across multiple Spot accounts and
organizations. In that mode, targets are read-only by default and every scale
request checks the current server-class minimum; a pool bidding below it is
immutable through Warden. See
[multiple accounts and grandfathered bids](docs/notes/multi-account-protection.md)
for configuration and scoped routes. The existing single-organization API
remains available for the current deployment.

> **Status — paused internal tooling:** the implementation is specific to the
> author's Rackspace Spot environment and has no public release. Its policy
> model is expected to move into [SEAM](https://github.com/jedarden/SEAM).

A policy-enforcing gateway that sits between the agent fleet and the Rackspace
Spot control-plane API. Agents ask warden to resize a node pool; warden holds
the Spot credential, enforces a hard envelope (max total nodes, allowed
server-class, bid cap), and forwards only what is inside that envelope. Agents
never see the Spot token.

warden is the interim, standalone form of a capability that belongs in
[SEAM](https://github.com/jedarden/SEAM) — a credential-injecting gateway fronting fleet upstreams. It is
built to be **absorbed**: its intent API becomes a SEAM route, and its policy
lifts into a SEAM route fragment. See `docs/plan/plan.md`.

## Why it exists

Rackspace Spot's IAM is org-scoped and coarse: a control-plane API token can
reshape *every* cloudspace in its org. To let autonomous NEEDLE workers scale
their own compute without also being able to reshape (or delete) production
clusters, two boundaries compose:

1. **A dedicated Spot org** holds only the worker cloudspaces — the blast-radius
   floor. Even total policy failure can't touch trading infra.
2. **warden** holds that org's token and enforces fine-grained invariants the
   coarse IAM can't — the ceiling.

## What it enforces

- **Max total nodes** across the whole org (default **10**) — a true ceiling
  that counts each pool's autoscaling max, not just its current size.
- **Allowed server classes** only (default `gp.vs1.medium-iad`).
- **Bid cap** (default `0.01`) — warden refuses to grow a pool bidding above it.
- **Fail closed on malformed pool state** — a pool with missing/negative
  `desired`, negative autoscaling bounds, or `maxNodes < minNodes` (in any pool
  of the org) blocks every scale until repaired out-of-band: an
  uninterpretable bound could under-count the org ceiling. See
  `docs/notes/invariant-policy.md`, "Malformed upstream state".
- **Autoscaled pools scale by ceiling** — a scale request on an autoscaled pool
  sets `autoscaling.maxNodes`, never `desired` (the upstream cluster-autoscaler
  owns it and would fight a fixed count) and never `minNodes`; a count below the
  pool's `minNodes` is denied. See `docs/notes/invariant-policy.md`.
- **No create / delete / re-shape** — impossible by construction: the intent API
  exposes no field for them.

## API

| Method | Path | Purpose |
|--------|------|---------|
| `GET`  | `/healthz` | Liveness (no auth) |
| `GET`  | `/v1/pools` | List pools with class, bid, and the acceptable node-count window (`lowerBound`/`upperBound`) |
| `POST` | `/v1/pools/{name}/scale` | Set node count on an existing pool: `{"count": N}` |

All `/v1` calls require a caller bearer token (`Authorization: Bearer <token>`).

**Error contract:** decisions come back as JSON with an `allowed` boolean;
transport/validation failures are plain text. Per-endpoint status codes,
response bodies, and the full 403 reason catalog are in
[`docs/notes/api.md`](docs/notes/api.md). In short:

| Status | Meaning | Retry? |
|--------|---------|--------|
| `400` | Malformed body (`count` must be an integer) | no |
| `401` | Missing/unknown caller token | no |
| `403` | **Any** policy denial — uniform code, discriminated by the JSON `reason` string | not as-is |
| `404` | Unknown pool (scale) | no — pools are created out-of-band |
| `409` | Every optimistic-concurrency retry lost; nothing applied | yes, later |
| `502` | Spot upstream failed or timed out | yes, later |

**Scale semantics:** on a fixed pool, `count` sets `spec.desired` (the size). On
an autoscaled pool it sets the autoscaler ceiling `spec.autoscaling.maxNodes` —
never `desired`, which the upstream cluster-autoscaler owns and would revert,
and never `autoscaling.minNodes`. A `count` below the pool's current
`autoscaling.minNodes` is denied (403), because it would invert the window;
lower the floor out-of-band first. That floor is visible to callers as
`lowerBound` on `GET /v1/pools` (0 on a fixed pool), paired with `upperBound`
— the count window the scale endpoint accepts. Scale-to-zero on an autoscaled
pool (`minNodes: 0`) sets `maxNodes: 0` — the pool is paused, not deleted.

## Configuration

All configuration is `WARDEN_*` environment variables, read once at startup;
invalid config exits `1` before listening. Required:
`WARDEN_SPOT_REFRESH_TOKEN`, `WARDEN_ORG_NAMESPACE` (must start `org-`), and
`WARDEN_CALLER_TOKENS`. Everything else defaults — org ceiling `10`, class
`gp.vs1.medium-iad`, bid cap `0.01` (raised from an earlier documented `0.001`
when Spot moved the floor; bead `warden-5787adc4`).

The deployment pins the namespace to the dedicated `apexalgo-agent` org
(`org-knyiltp8zznvkz5g`) and the class allowlist to the real pool's class
(`ch.vs1.large-ord`) rather than the code default — the defaults-vs-deployed
table, the full variable reference, and why the ceiling's accounting requires
the single namespace are in [`docs/notes/configuration.md`](docs/notes/configuration.md).

## Structure

- `cmd/warden/` — entrypoint
- `internal/policy/` — the enforcement core (pure, exhaustively tested)
- `internal/spot/` — Rackspace Spot API client + OAuth token manager
- `internal/server/` — the intent API + caller auth + audit
- `deploy/` — manifests for the rs-manager deployment, **live since 2026-07-30** (image `ronaldraygun/warden:0.1.0`, reachable at `warden-rs-manager.ardenone.com:8444` over the tailnet). Source of truth is `declarative-config` at `k8s/rs-manager/warden/`, synced by ArgoCD Application `warden-ns-rs-manager`; this copy is a staging mirror — never applied with `kubectl` directly
- `docs/notes/` — security model, invariant policy, API error contract, configuration reference
- `docs/research/` — Rackspace Spot API reference
- `docs/plan/plan.md` — the complete plan
