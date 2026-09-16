# Intent API — error contract

The status codes and response bodies callers (NEEDLE workers, the future
autoscaler) should program against. Endpoint list and scale semantics live in
the README; the enforcement reasons behind the 403s live in
[`invariant-policy.md`](invariant-policy.md); the auth model lives in
[`security-model.md`](security-model.md).

## Two response shapes

- **JSON** (`Content-Type: application/json`) — every *decision* warden returns:
  scale outcomes (200/403/409) and the pool list (200). Decisions carry an
  `"allowed"` boolean and a human-readable `"reason"`.
- **Plain text** (`text/plain; charset=utf-8`, trailing newline — Go's
  `http.Error`) — every *transport / validation* failure: 400, 401, 404, 405,
  502, and the mux's 404/405 for bad routes. These bodies are a short message,
  never JSON.

Authentication runs before anything else on both `/v1` endpoints: an
unauthenticated request with a malformed body gets 401, not 400.

## `GET /healthz`

Always `200` with body `ok` when the process is up — no auth, no upstream
check (it backs the liveness/readiness probes; it does not mean Spot is
reachable).

## `GET /v1/pools`

| Status | Body | When |
|--------|------|------|
| `200` | JSON (below) | Pools listed from the org namespace |
| `401` | `missing bearer token` or `unauthorized` | No/!`Bearer` scheme header, or token not in the allowlist |
| `502` | `upstream error` | Spot unreachable, OAuth exchange failed, non-200 from Spot, or per-request timeout |

`200` body:

```json
{
  "pools": [
    {
      "name": "agent-pool",
      "serverClass": "ch.vs1.large-ord",
      "bidPrice": "0.01",
      "upperBound": 4,
      "autoscaled": true
    }
  ],
  "cap": 10
}
```

`cap` is the configured org-wide node ceiling (`WARDEN_MAX_TOTAL_NODES`).
`upperBound` is what the ceiling sums: `autoscaling.maxNodes` when `autoscaled`,
else the fixed `desired`.

## `POST /v1/pools/{name}/scale`

Request body `{"count": N}` (64 KiB max). Check order: auth (401) → body
decode (400) → list pools (502) → pool exists (404) → policy (403) → apply
(200/409/502).

| Status | Body | When |
|--------|------|------|
| `200` | JSON `{"allowed":true,"pool":…,"count":…,"reason":…}` | Scale applied |
| `400` | `invalid body: <detail>` | Body is not valid JSON, exceeds 64 KiB, or `count` is present but not an integer (`"5"`, `1.5`) |
| `401` | `missing bearer token` or `unauthorized` | As above |
| `403` | JSON `{"allowed":false,"reason":"…"}` | **Every** policy denial — see below |
| `404` | `pool not found` | No pool with that name in the org namespace |
| `409` | JSON `{"allowed":false,"reason":"pool changed concurrently on every attempt; retry later"}` | All `maxScaleAttempts` optimistic-concurrency retries lost the race; nothing was applied |
| `502` | `upstream error` / `upstream error applying scale` | Spot list/patch failed (incl. timeout, OAuth failure) |

Body-decoding details that surprise callers:

- Unknown JSON fields are ignored (no `DisallowUnknownFields`).
- A **missing** `count` decodes as `0` and is *not* a 400 — it is a legal
  scale-to-zero request (and on an autoscaled pool with `minNodes > 0` it comes
  back as the 403 below-minNodes denial).

## Denials: uniform 403, distinguished by `reason`

There is **no distinct status code per policy violation**. All of these are the
same `403` with `{"allowed": false, "reason": …}`; the `reason` string is the
only discriminator:

| Denial | `reason` pattern |
|--------|------------------|
| Negative count | `count must be >= 0, got %d` |
| Below autoscaling floor | `count %d is below autoscaling minNodes %d; warden sets maxNodes only and will not lower minNodes` |
| Class not allowlisted | `server class %q is not in the allowlist` |
| Unparseable bid (fail closed) | `cannot parse pool bidPrice %q (fail closed)` |
| Bid above cap | `pool bidPrice %.6f exceeds cap %.6f` |
| Org ceiling exceeded | `request would bring org node ceiling to %d, exceeding cap of %d` |

`reason` strings are diagnostic, not a stable enum — match on the status code,
log the reason. Unknown pool is deliberately *not* folded into the 403: it is a
`404` (the request never reached a policy decision about a real pool).

Every scale decision — allow, deny, per-retry re-decision, and 404 — is
audited (see [`security-model.md`](security-model.md)), so a retried request
leaves one audit entry per re-decision.

## Caller retry guidance

| Status | Retry? |
|--------|--------|
| `400` | No — fix the body |
| `401` | No — credentials are wrong; re-provision out-of-band |
| `403` | Not as-is — lower `count` or fix the pool out-of-band (class/bid/minNodes); the ceiling denial may pass later if other pools shrink |
| `404` | No — the pool does not exist; pools are created out-of-band (console/Terraform), never through warden |
| `409` | Yes — later; warden already retried internally (`maxScaleAttempts = 4`, a compile-time constant, not configurable) and failed closed |
| `502` | Yes — later; upstream Spot was unavailable or timed out |

## Routing (no default-allow)

Unknown paths get the mux's plain-text `404`; a known path with the wrong
method gets plain-text `405` with an `Allow` header. There is no catch-all
handler that could be mistaken for an allowed operation.
