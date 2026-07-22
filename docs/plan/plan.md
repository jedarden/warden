# warden Plan

## Overview

warden is a policy-enforcing gateway between the agent fleet and the Rackspace
Spot control-plane API. It holds the Spot org credential and exposes a narrow
intent API (list pools, scale an existing pool) with a hard, invariant envelope:
max total nodes, an allowed server-class set, and a bid cap. It is the interim,
standalone form of a SEAM route, built to be absorbed into SEAM once SEAM ships.

## Context & motivation

The fleet wants elastic NEEDLE-worker compute on Rackspace Spot (medium class,
~$0.001/hr floor bid). Programmatically reshaping a Spot cluster requires the
**organization-scoped** control-plane API, and Spot's IAM is coarse — one org
token can reshape or delete every cloudspace in the org. Handing that token to
autonomous agents is unacceptable if the org also contains production trading
clusters.

Two boundaries compose to make it safe:

1. **Dedicated Spot org** (created out-of-band, in the Spot console): contains
   only worker cloudspaces. This is the blast-radius *floor* — even total policy
   failure cannot reach trading infra, because those clusters aren't in the
   token's org.
2. **warden**: holds the dedicated org's token and enforces the fine-grained
   invariants the coarse IAM cannot. This is the *ceiling*.

Neither alone is sufficient: the org without warden is coarse; warden without
the org is a juicy single point of failure.

## Architecture

```
NEEDLE workers / autoscaler          warden (in rs-manager, tailnet-only)         Rackspace Spot API
─────────────────────────           ─────────────────────────────────           ──────────────────
  caller bearer token   ──HTTP──▶   auth ─▶ policy ─▶ construct patch  ──OAuth──▶  ngpc.rxt.io/v1
  (no Spot token ever)              │        │         (count only)      bearer     SpotNodePool
                                    └─ audit every decision            (injected)
```

- **Transport in:** small HTTP intent API over the tailnet (agents never hold a
  Spot token; the token lives only in warden — credential separation is the
  primary control, network isolation is secondary).
- **Transport out:** Rackspace Spot public API — Kubernetes-style CRDs under
  `ngpc.rxt.io/v1`, base `https://spot.rackspace.com`, OAuth bearer obtained by
  exchanging a refresh token at `login.spot.rackspace.com/oauth/token`.
- **Enforcement:** warden constructs its own merge-patch touching only the node
  count, so class/bid/create/delete cannot be expressed through it.

## Components

- `internal/config` — env-driven config + validation (secrets via SealedSecret).
- `internal/spot` — Spot API client, OAuth refresh-token manager (cached,
  auto-refreshed), list/get/scale of SpotNodePools.
- `internal/policy` — pure enforcement core: `EvaluateScale` applies class
  allowlist, bid cap, and org-wide node-ceiling accounting; fails closed.
- `internal/server` — intent API (`GET /v1/pools`, `POST /v1/pools/{name}/scale`),
  constant-time caller-token auth, audit logging.
- `internal/audit` — structured decision log (allow + deny).
- `cmd/warden` — wiring + graceful shutdown.
- `deploy/` — manifests staged for `declarative-config`.

## Data models

`SpotNodePool` (partial, `ngpc.rxt.io/v1`): `spec.serverClass`, `spec.bidPrice`,
`spec.desiredCount` (fixed pools), `spec.autoscaling.{enabled,minNodes,maxNodes}`
(autoscaled pools). A pool's **UpperBound** = `maxNodes` when autoscaling is on,
else `desiredCount` — this is what the org-wide cap sums, making the cap a true
ceiling rather than a snapshot.

> Field names are from the public-API docs and must be verified against a live
> `GET spotnodepools/<name>` in the real org before go-live (see
> `internal/spot/types.go`).

## Enforcement invariants

For a request to scale pool `P` to `count`:

1. `count >= 0`.
2. `P.serverClass ∈ allowlist` (default `{gp.vs1.medium-iad}`).
3. `P.bidPrice <= maxBid` (default `0.001`); unparseable bid ⇒ deny.
4. `count + Σ UpperBound(other pools) <= maxTotalNodes` (default `10`).

Impossible by construction (no endpoint / field): create pool, delete pool,
create/delete cloudspace, change server class, change bid.

## Security model

- Spot token held only by warden, from a tailnet-served SealedSecret in
  rs-manager; agents authenticate with a separate, narrow caller token that
  grants nothing but bounded intent calls.
- Caller tokens compared in constant time against sha256 digests; never logged.
- Deploy is non-root, read-only rootfs, all caps dropped.
- Exposed only on the cluster's single Tailscale/Traefik ingress — no public
  internet.

See `docs/notes/security-model.md`.

## Implementation phases

- [x] Phase 1: Enforcement core (policy engine + tests), Spot client, intent API,
      audit, config. Builds, vets, tests green.
- [ ] Phase 2: Field-name verification against the live org CRD; wire the real
      refresh token via SealedSecret.
- [ ] Phase 3: Containerize (Argo `warden-build` WorkflowTemplate → `ronaldraygun/warden`,
      pinned digest) and deploy via `declarative-config` (`k8s/rs-manager/warden/`),
      tailnet-only IngressRoute.
- [ ] Phase 4: Point a deterministic autoscaler (or the NEEDLE dispatcher) at
      warden; confirm scale-up / scale-to-zero end to end against a real pool.
- [ ] Phase 5: Preemption-safety in the workers (release bead + clean worktree
      on node loss) — prerequisite for leaning on floor-bid Spot.
- [ ] Phase 6 (absorption): expose the intent API as a SEAM route; move the token
      and policy behind SEAM; retire warden's standalone auth in favor of SEAM
      per-agent identity (SEAM Phase 7 / NEEDLE tsnet identity).

## Open questions

- Exact SpotNodePool CRD field names (see Phase 2).
- One pool per worker vs. one pool autoscaled 0..N — affects how "scale" maps to
  intent calls and how preemption churns. Leaning autoscaled pool(s) with warden
  capping `maxNodes`.
- Does the deterministic autoscaler live in-cluster (rs-manager) or as a fleet
  process? Either way it holds only a caller token, never the Spot token.
- Shared compile cache (sccache → B2/S3) for cold Rust builds on ephemeral nodes
  — not warden's job, but a hard dependency for the economics to hold.
