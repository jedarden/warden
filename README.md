# warden

A policy-enforcing gateway that sits between the agent fleet and the Rackspace
Spot control-plane API. Agents ask warden to resize a node pool; warden holds
the Spot credential, enforces a hard envelope (max total nodes, allowed
server-class, bid cap), and forwards only what is inside that envelope. Agents
never see the Spot token.

warden is the interim, standalone form of a capability that belongs in
[SEAM](../SEAM) — a credential-injecting gateway fronting fleet upstreams. It is
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
- **Bid cap** (default `0.001`) — warden refuses to grow a pool bidding above it.
- **No create / delete / re-shape** — impossible by construction: the intent API
  exposes no field for them.

## API

| Method | Path | Purpose |
|--------|------|---------|
| `GET`  | `/healthz` | Liveness (no auth) |
| `GET`  | `/v1/pools` | List pools with class, bid, node ceiling |
| `POST` | `/v1/pools/{name}/scale` | Set node count on an existing pool: `{"count": N}` |

All `/v1` calls require a caller bearer token (`Authorization: Bearer <token>`).

## Structure

- `cmd/warden/` — entrypoint
- `internal/policy/` — the enforcement core (pure, exhaustively tested)
- `internal/spot/` — Rackspace Spot API client + OAuth token manager
- `internal/server/` — the intent API + caller auth + audit
- `deploy/` — manifests staged for `declarative-config` (GitOps; not applied directly)
- `docs/notes/` — security model, invariant policy
- `docs/research/` — Rackspace Spot API reference
- `docs/plan/plan.md` — the complete plan
