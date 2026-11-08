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
| `502` | `upstream error` | Spot unreachable, OAuth exchange failed, non-200 from Spot, per-request timeout, or a 200 body that fails to decode as a pool list (e.g. a count field that is not an integer) |

`200` body:

```json
{
  "pools": [
    {
      "name": "agent-pool",
      "serverClass": "ch.vs1.large-ord",
      "bidPrice": "0.01",
      "lowerBound": 2,
      "upperBound": 4,
      "autoscaled": true
    }
  ],
  "cap": 10
}
```

`cap` is the configured org-wide node ceiling (`WARDEN_MAX_TOTAL_NODES`).
`upperBound` is what the ceiling sums: `autoscaling.maxNodes` when `autoscaled`,
else the fixed `desired`. `lowerBound` is the scale-request floor: counts below
it are denied with the below-minNodes 403 (see the denial table below). It is
`autoscaling.minNodes` when `autoscaled`, else `0` — on a fixed pool any
non-negative count is legal. Together `lowerBound ≤ count ≤ upperBound` is the
count window a caller can program against; a count inside it can still be
denied by the org ceiling, the class allowlist, or the bid cap. Lowering a
floor is a pool re-shape done out-of-band (console/Terraform); warden never
writes `minNodes`.

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
| `502` | `upstream error` / `upstream error applying scale` | Spot list/patch failed (incl. timeout, OAuth failure), or the pool list failed to decode (a count field that is not an integer — never reaches policy) |

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
| Class not allowlisted on a growth | `refusing to grow out-of-policy pool: server class %q is not in the allowlist` |
| Unparseable bid on a growth | `refusing to grow out-of-policy pool: cannot parse pool bidPrice %q (fail closed)` |
| Bid above cap on a growth | `refusing to grow out-of-policy pool: pool bidPrice %.6f exceeds cap %.6f` |
| Org ceiling exceeded by a growth | `request would bring org node ceiling to %d, exceeding cap of %d` |
| Existing class/bid/org drift on a hold | `out-of-policy pool requires a strict reduction: requested count %d is not below current bound %d (<drift>)` |
| Malformed target pool state (fail closed) | `target pool state is malformed: <detail> (fix the pool out-of-band; warden will not act on state it cannot interpret)` — `<detail>` is one of: `fixed pool has no desired count`, `desired count %d is negative`, `autoscaling minNodes %d is negative`, `autoscaling maxNodes %d is negative`, `autoscaling maxNodes %d is below minNodes %d` |
| Malformed bystander pool (fail closed) | `org snapshot has malformed pool state: pool %q: <detail> (fail closed)` — same `<detail>` set, but the named pool is *not* the one being scaled; every scale in the org is denied until it is fixed |

The last two rows are the malformed-upstream-state rule
([`invariant-policy.md`](invariant-policy.md), "Malformed upstream state"):
a pool whose node-count fields cannot be interpreted could under-count the
org ceiling, so no scale is decided against a snapshot containing one. Unlike
the other 403s, no `count` change can help — only an out-of-band repair of
the named pool. A count field that is invalid at the wire level
(`"desired": "three"`, `1.5`) is one step earlier still: the pool list
itself fails to decode, so no policy decision — 502 `upstream error`, no
audit entry, and *retryable* once upstream decodes again, unlike the 403s.

`reason` has no separate `reason_code` field. For malformed policy denials,
the following names are stable contract terms: the outer reason family is
`target pool state is malformed` when the requested pool is broken, or
`org snapshot has malformed pool state` when a bystander pool is broken; the
detail labels in the table are stable and only their numeric/name values are
interpolated. Callers should still branch on the status (`403`) and
`allowed:false`, then retain the full reason for diagnosis. Unknown pool is
deliberately *not* folded into the 403: it is a `404` (the request never
reached a policy decision about a real pool).

### Existing non-compliant pools: direction, reason, and patch

For an existing target with a disallowed class, an over-cap or unparseable
configured bid, or an organization already above its summed upper-bound
ceiling, policy is monotonic remediation rather than an unconditional ban:

| Request | Response | Upstream effect |
|---|---|---|
| `count < current UpperBound` (including fixed scale-to-zero) | `200`; reason contains `strict out-of-policy reduction permitted` and the drift/current requested org total | exactly one count-only patch, carrying the listed pool's `metadata.resourceVersion` |
| `count < current maxNodes` on autoscaled pool with `minNodes = 0` | `200`; same strict-remediation reason | exactly one patch of `spec.autoscaling.maxNodes`; `desired`, `minNodes`, class, and bid are untouched |
| `count == current UpperBound` | `403`; reason contains `out-of-policy pool requires a strict reduction` | zero patches — an equal-count hold remediates nothing |
| `count > current UpperBound` | `403`; growth names class/bid drift or the org-ceiling reason | zero patches |

An autoscaled count below its current `minNodes` remains a 403 even when the
pool is otherwise out of policy; scale-to-zero requires `minNodes = 0`. A
reduction is allowed even if other pools leave the resulting org total above
the configured ceiling: the target bound still fell strictly. This exception
does not apply to malformed node-count state, missing bids, failed class reads,
or a valid bid below the current server-class new-bid minimum; those are
denied in every direction. A non-empty unparseable bid is the exception within
bid parsing: strict reduction may wind it down, but hold and growth deny.

### Malformed-state API and audit contract

The malformed-state rows have one explicit response and audit contract. The
requested pool is always the API path target; a malformed bystander named in
the reason does not change the response's `pool` audit field. `count` is always
the caller's requested count, including when it is negative or would have
repaired an inverted window.

| State | Stable API reason name | Response | Audit event | Retry / mutation |
|---|---|---|---|---|
| Fixed `desired` absent or `null` | `target pool state is malformed` or `org snapshot has malformed pool state`; detail `fixed pool has no desired count` | `403` JSON denial | exactly one `msg:"audit"`, `action:"scale"`, `allowed:false` with target `pool`, requested `count`, and full `reason` | no automatic retry; zero upstream patches; repair out-of-band, then re-request |
| Fixed `desired` negative | same family; detail `desired count %d is negative` | `403` JSON denial | same event and fields | no automatic retry; zero upstream patches; repair out-of-band, then re-request |
| Autoscaled `minNodes` negative | same family; detail `autoscaling minNodes %d is negative` | `403` JSON denial | same event and fields | no automatic retry; zero upstream patches; repair out-of-band, then re-request |
| Autoscaled `maxNodes` negative | same family; detail `autoscaling maxNodes %d is negative` | `403` JSON denial | same event and fields | no automatic retry; zero upstream patches; repair out-of-band, then re-request |
| Autoscaled `maxNodes < minNodes` | same family; detail `autoscaling maxNodes %d is below minNodes %d` | `403` JSON denial | same event and fields | no automatic retry; zero upstream patches; repair out-of-band, then re-request |
| Count field present but not an integer (`"three"`, `1.5`, `true`) | no policy reason: decode fails before policy | `502` plain text `upstream error` | no `msg:"audit"` event because no authorization decision occurred | retry later; zero upstream patches |

For the five policy rows, the target/bystander family is selected by which pool
is malformed, but the audit `pool` remains the pool named in the request path.
The audit event's complete payload is `caller`, `remote_addr`, `action`,
`account`, `namespace`, `pool`, `count`, `allowed`, and `reason`; `caller` is
the token fingerprint, never the raw token. The event name `audit` and
`action="scale"` are stable. A malformed policy decision returns before the
write/retry loop, so malformed input cannot mutate upstream state.

Example target denial:

```http
POST /v1/pools/agent-pool/scale HTTP/1.1
Authorization: Bearer <token>
Content-Type: application/json

{"count":9}
```

```http
HTTP/1.1 403 Forbidden
Content-Type: application/json

{"allowed":false,"reason":"target pool state is malformed: autoscaling maxNodes 2 is below minNodes 8 (fix the pool out-of-band; warden will not act on state it cannot interpret)"}
```

The corresponding audit record has `pool:"agent-pool"`, `count:9`,
`allowed:false`, `action:"scale"`, `msg:"audit"`, and the same complete
reason. If `pool-b` were the malformed bystander, the reason would name
`pool-b` but the audit record would still carry `pool:"agent-pool"`.

Every scale decision — allow, deny, per-retry re-decision, and 404 — is
audited (see [`security-model.md`](security-model.md)), so a retried request
leaves one audit entry per re-decision.

## Caller retry guidance

| Status | Retry? |
|--------|--------|
| `400` | No — fix the body |
| `401` | No — credentials are wrong; re-provision out-of-band |
| `403` | Not as-is — for an existing class/bid/org violation, request a strictly lower `count` (including zero when the floor permits it); an equal-count hold is denied. Fix the pool out-of-band when the reason names malformed state, a missing bid, a below-floor count, or a grandfathered bid. `lowerBound` on the pool list shows the autoscaling floor. Warden does not retry a policy deny (one audited deny, zero patches) |
| `404` | No — the pool does not exist; pools are created out-of-band (console/Terraform), never through warden |
| `409` | Yes — later; warden already retried internally (`maxScaleAttempts = 4`, a compile-time constant, not configurable) and failed closed |
| `502` | Yes — later; upstream Spot was unavailable or timed out |

## Routing (no default-allow)

Unknown paths get the mux's plain-text `404`; a known path with the wrong
method gets plain-text `405` with an `Allow` header. There is no catch-all
handler that could be mistaken for an allowed operation.
