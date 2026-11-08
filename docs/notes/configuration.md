# Configuration reference

The current deployment uses legacy single-target mode. For multiple Spot
accounts or organizations, use the explicit target configuration and scoped
routes in [multi-account-protection.md](multi-account-protection.md), with the
credential wiring and GitOps rollout in
[spot-token-provisioning.md](spot-token-provisioning.md). Do not point
`WARDEN_ORG_NAMESPACE` at a second org to add it: that replaces the only
legacy target.

warden is configured entirely through `WARDEN_*` environment variables, read
once at startup by `internal/config`. There is no config file and no runtime
reload: an invalid value fails `config.Load`, `cmd/warden` logs it and exits
`1` before listening — fail fast, fail closed. Changing the envelope
(`MAX_TOTAL_NODES`, `ALLOWED_SERVER_CLASSES`, `MAX_BID_PRICE`, …) is a
`declarative-config` commit + ArgoCD sync, never a live toggle (see
[`invariant-policy.md`](invariant-policy.md), "Changing the envelope").

## Required variables by mode

| Variable | Constraint |
|----------|------------|
| `WARDEN_SPOT_REFRESH_TOKEN` | Legacy mode only: the Spot org refresh token, injected via `warden-spot-credentials` ExternalSecret. Warden exchanges it for the OIDC `id_token`. |
| `WARDEN_ORG_NAMESPACE` | Legacy mode only: must start with `org-`. See "Org namespace" below. |
| `WARDEN_TARGETS_JSON` | Target mode selector: a non-empty JSON array of configured orgs with `refreshTokenEnv` references, never token values or OpenBao paths. Each referenced `WARDEN_SPOT_REFRESH_TOKEN_<ALIAS>` must be set. |
| `WARDEN_CALLER_TOKENS` | Comma-separated, at least one after whitespace-trimming. Injected via envFrom the `warden-caller-tokens` ExternalSecret (OpenBao). Raw values are never logged; only sha256 fingerprints appear in audit records. Generation, rotation, revocation, and rollback: [`caller-token-provisioning.md`](caller-token-provisioning.md). |

Missing or invalid ⇒ startup error naming the variable
(`invalid config: WARDEN_… is required; …`).

## All variables

The `WARDEN_MAX_TOTAL_NODES`, `WARDEN_MAX_BID_PRICE`, and
`WARDEN_ALLOWED_SERVER_CLASSES` values below describe the legacy envelope.
Target mode takes those limits from each scaling target's JSON entry.

| Variable | Default | Required | Meaning |
|----------|---------|----------|---------|
| `WARDEN_LISTEN_ADDR` | `:8080` | no | HTTP listen address |
| `WARDEN_SPOT_BASE_URL` | `https://spot.rackspace.com` | no | Spot ngpc API base |
| `WARDEN_SPOT_TOKEN_URL` | `https://login.spot.rackspace.com/oauth/token` | no | OAuth refresh-token endpoint |
| `WARDEN_SPOT_CLIENT_ID` | Spot's public ngpc client ID (hardcoded; a public identifier, not a credential — see `internal/config/config.go`) | no | OAuth `client_id` for the refresh grant |
| `WARDEN_SPOT_REFRESH_TOKEN` | — | legacy mode | The legacy org credential (see above) |
| `WARDEN_ORG_NAMESPACE` | — | legacy mode | The legacy Spot org namespace (see below) |
| `WARDEN_TARGETS_JSON` | unset | target mode | Non-empty JSON array selecting target mode, even with one entry; contains env references, not credentials |
| `WARDEN_SPOT_REFRESH_TOKEN_<ALIAS>` | — | for each target account | One distinct, non-empty env value per account alias; organizations on the same account share its reference |
| `WARDEN_MAX_TOTAL_NODES` | `10` | no | Org-wide node ceiling; must be `>= 0` |
| `WARDEN_MAX_BID_PRICE` | `0.01` | no | Bid cap — see the drift note below |
| `WARDEN_ALLOWED_SERVER_CLASSES` | `gp.vs1.medium-iad` | no | Comma-separated allowlist; must resolve to at least one class |
| `WARDEN_CALLER_TOKENS` | — | **yes** | Caller bearer-token allowlist (see above) |
| `WARDEN_REQUEST_TIMEOUT` | `30s` | no | Go duration; doubles as the Spot client's HTTP timeout and the per-request handler deadline. Exceeding it surfaces as `502` to the caller. |

Spot price guard (optional; see [`spot-price-guard.md`](spot-price-guard.md)):

| Variable | Default | Required | Meaning |
|----------|---------|----------|---------|
| `WARDEN_SPOT_GUARD_JSON` | unset (no guard) | no | Guard target, threshold, timing, `dryRun` (defaults to `true`) |
| `WARDEN_SPOT_REFRESH_TOKEN_<ALIAS>` | — | with the guard | The guarded account's refresh token, named by the guard's `refreshTokenEnv` (deployed: `..._APEXALGO`, main org); reuse this env reference if that account is also a caller target |
| `WARDEN_ALERT_NTFY_URL` | — | with the guard | Existing ntfy paging topic URL — a credential, never logged |
| `WARDEN_ALERT_NTFY_TOKEN` | empty | no | Optional ntfy bearer token |

Not configurable (compile-time constants):

- `maxScaleAttempts = 4` in `internal/server/server.go` — the
  optimistic-concurrency retry budget (one planned attempt + three
  re-decisions). Exhaustion ⇒ `409` to the caller
  ([`api.md`](api.md)).
- `MaxGuardRestoreCount = 2` in `internal/config/guard.go` — the most nodes
  the price guard may ever write (user decision 2026-10-11).

## Empty-value semantics

`env()` treats an empty variable exactly like an unset one and applies the
default — so setting `WARDEN_ALLOWED_SERVER_CLASSES=""` selects the default
allowlist rather than an empty one. To express a genuinely empty *list*, pass a
value that splits to nothing (e.g. `","`), which then fails validation
("must list at least one class") — an empty allowlist denies everything and is
refused at startup rather than silently accepted. Same for an all-whitespace
`WARDEN_CALLER_TOKENS`.

## Defaults vs deployed values

The code defaults and the deployed envelope differ; the deployment is
authoritative (`deploy/deployment.yaml` → `declarative-config`):

| Setting | Code default | Deployed |
|---------|--------------|----------|
| `WARDEN_MAX_TOTAL_NODES` | `10` | `10` |
| `WARDEN_ALLOWED_SERVER_CLASSES` | `gp.vs1.medium-iad` | `ch.vs1.large-ord` — matches the real pool on the `agent-sandbox` cloudspace (us-central-ord-1) |
| `WARDEN_MAX_BID_PRICE` | `0.01` | `0.01` |

**Bid-cap drift:** some early docs said `0.001`; the enforced default has been
`0.01` since Spot raised the gp.vs1.medium floor 10× (verified via the
serverclasses API, 2026-07-27). Code, deployment, and all docs now agree on
`0.01` — bead `warden-5787adc4` has the history.

## Org namespace in the deployed legacy mode

**warden operates against the dedicated `apexalgo-agent` Spot org, namespace
`org-knyiltp8zznvkz5g`** (set in `deploy/deployment.yaml`; confirmed via the
2026-07-27 live-API probe). The identifier itself is not a secret — without
that org's refresh token it grants nothing.

This single-namespace scope is load-bearing in legacy mode:

- Every list and every scale patch is issued against
  `…/namespaces/<WARDEN_ORG_NAMESPACE>/spotnodepools` and nothing else —
  pools in any other org/namespace are invisible to warden and unreachable
  through it.
- The org-wide node ceiling is computed by summing `UpperBound` over the pools
  returned by that one list call. The cap is a true ceiling **only because all
  scale-able pools live in this one namespace**; a pool outside it would be
  uncounted. In target mode each configured org has its own namespace and
  independent scaling envelope; one Warden instance may serve several.

This is the enforcement-layer complement of the blast-radius floor: the
dedicated org (see [`security-model.md`](security-model.md)) is what bounds
what the token can touch; the namespace pin is what makes the ceiling's
accounting complete.
