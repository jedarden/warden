# Spot price guard

**User decision 2026-10-11:** iad-ci `mh.vs1.large-iad` pool, bid `0.015`, guard
off above `0.012`/h, auto-restore after 30 min, max 2 nodes. Bead
`warden-faf60f86`.

warden runs an internal price guard for one pool in the **main** Spot
organization (`org-kselolwaoxl3zxfm`, which also holds the production trading
clusters): the single fixed pool in cloudspace `iad-ci` with server class
`mh.vs1.large-iad`. The pool is created by a human in the Spot console; the
guard never creates, deletes or re-shapes anything.

## Behaviour

Every poll (60 s) the guard reads `ServerClass/mh.vs1.large-iad`
`status.spotPricing.marketPricePerHour` and lists the organization's pools.

| Situation | Guard does |
|---|---|
| Market **strictly above** `0.012` and the pool is above 0 | Sets `spec.desired` to 0, records the off as its own, alerts **OFF** |
| Market exactly `0.012` or below | Nothing (at the threshold is not above it) |
| After its own off: market at or below `0.012` at every poll for 30 continuous minutes | Sets `spec.desired` to 2, alerts **RESTORED** |
| Spike, unreadable price, or a gap in observations while holding | Restarts the 30-minute timer |
| Someone else changed the pool after the guard's off (count, bid, class, cloudspace, autoscaling, any spec edit — detected by `metadata.generation`) | Drops its claim, alerts **released**, never restores it |
| Someone else changes the pool during a spike (e.g. scales it up deliberately) | Hands off until the price is back at or below the threshold; protects again on the next spike |
| Pool at 0 that the guard did not turn off | Never restored |
| Pool was at 1 before the guard's off | Turned off, but not auto-restored (the permit allows only 0 or 2); after 30 quiet minutes the guard alerts "restore it manually" and releases |
| Pool was at 3 or more before the off | Restored to 2, never more |
| Price or pool list unreadable, no matching pool ("waiting for pool"), more than one matching pool, pool autoscaled / missing `desired` / missing `resourceVersion`, state unreadable or unwritable | **Fail static**: changes nothing, alerts **FAIL-STATIC** once per condition |

Every condition change is alerted once; a condition that persists is not
re-alerted (also across restarts — the last alerted condition is part of the
persisted state). Actions (off, restore, release) are always alerted.

Known limits: a human who sets the pool to 0 while the guard already holds it
at 0 makes a no-op write, which the API does not record (no generation bump),
so the guard cannot see it. To keep the pool off after a guard off, change
anything else in its spec (or scale to 1 and back to 0). The guard reacts to
price, not to Spot preemption — a market above the bid (`0.015`) preempts the
nodes regardless.

## Permission — narrower than the caller API

- The guarded organization is **not a caller target**. In the deployed
  single-organization mode callers can only reach the apexalgo-agent org; in
  multi-target mode config validation refuses `allowScale: true` on the guarded
  org (`internal/config/guard.go`). Callers gain no ability to list or scale
  main-org pools.
- The guard's only write handle is a `Permit` (`internal/guard/permit.go`): one
  organization, the single pool matching `cloudSpace`+`serverClass`, a count of
  exactly 0 or `restoreCount`, a fixed (non-autoscaled) pool, and the snapshot's
  `resourceVersion` as a precondition. Anything else is refused before a
  request is sent.
- The write itself (`spot.Client.SetFixedDesired`) patches only `spec.desired`
  plus the `resourceVersion` precondition; class, bid, cloudspace and
  autoscaling cannot be expressed.
- `restoreCount` is capped at the compile-time constant
  `config.MaxGuardRestoreCount = 2`.
- In dry-run mode the Permit is built **without a writer**: no code path can
  reach a Spot write.
- The guard uses its own Spot client, never one of the caller targets'.

## Configuration

`WARDEN_SPOT_GUARD_JSON` (unset = no guard):

```json
{"account":"apexalgo","namespace":"org-kselolwaoxl3zxfm",
 "refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_APEXALGO",
 "cloudSpace":"iad-ci","serverClass":"mh.vs1.large-iad",
 "offAbovePerHour":"0.012","restoreAfter":"30m","restoreCount":2,
 "pollInterval":"60s","dryRun":true,"stateConfigMap":"warden-spot-guard-state"}
```

`dryRun` defaults to **true** when omitted. Validation fails startup (exit 1)
on any unknown field, `restoreCount` outside 1–2, a non-decimal or zero
threshold, `restoreAfter` < 1m or < 2 polls, a poll outside 10s–5m, a missing
credential, or a missing alert route — a guard that cannot alert does not
start.

Secrets arrive through the `warden-spot-guard` ExternalSecret (rs-manager
OpenBao, `openbao` ClusterSecretStore), by reference only:

| Env var | OpenBao path (field) |
|---|---|
| `WARDEN_SPOT_REFRESH_TOKEN_APEXALGO` | `secret/rs-manager/rs-manager/rackspace-spot/apexalgo/api-key` (field `key`) — the main-org refresh token; the org claim of the identity it mints is the main org (verified 2026-10-11) |
| `WARDEN_ALERT_NTFY_URL` | `secret/rs-manager/iad-ci/cnpg-backup-watchdog` (`ntfy-url`) |
| `WARDEN_ALERT_NTFY_TOKEN` | `secret/rs-manager/iad-ci/cnpg-backup-watchdog` (`ntfy-token`, currently empty) |

## Alert route

The guard publishes directly to the fleet's existing ntfy paging topic — the
same OpenBao path the cnpg-backup-watchdog and the rs-manager and iad-ci
Alertmanagers deliver to. No new channel. The topic URL is a credential: it is
never logged and transport errors are stripped of it. Each delivery logs
`spot guard alert delivered` with the `ntfy_id` ntfy returned (the delivery
evidence); a failure logs `spot guard alert delivery failed` after three
attempts. Titles start `warden spot guard iad-ci/mh.vs1.large-iad:`; in dry-run
they start `[DRY-RUN]`.

## State

Persisted in ConfigMap `warden-spot-guard-state` (key `state.json`) in the
`warden` namespace, written by warden through its ServiceAccount (Role
`warden-spot-guard-state`: `get`/`update` on that name, `create` on
configmaps). It is created by warden, not ArgoCD, and holds no credential.
Every update carries the ConfigMap's `resourceVersion`, so a second instance
fails static instead of overwriting; the Deployment rolls with `maxSurge: 0`
/ `maxUnavailable: 1`, so the old pod is stopped before the new one starts. The intent to write is persisted **before** each
Spot write, and an unconfirmed write is reconciled on the next poll from the
pool's `metadata.generation` (+1 = the guard's write landed; unchanged = it did
not; anything else = someone else, release). Read it without writing:

```bash
kubectl --server=http://traefik-rs-manager:8001 -n warden get configmap warden-spot-guard-state -o jsonpath='{.data.state\.json}' | jq .
kubectl --server=http://traefik-rs-manager:8001 -n warden logs deploy/warden | grep 'spot guard'
```

## Rollout

1. Deploy with `"dryRun":true`. Confirm from the logs that each tick's
   `condition`/`action` is right against live data (before the pool exists:
   `fail-static:waiting-for-pool`, one alert).
2. Enable writes in a **separate** declarative-config commit
   (`"dryRun":true` → `"dryRun":false`).
3. Run the fire drill below once the pool exists, **before** any iad-ci
   workload moves onto it.

Done 2026-10-11 for steps 1–2: image `0.3.0` (`warden-build-gfggr`, tag
`v0.3.0`, digest `sha256:e2d9b1c4…`); declarative-config `eaca9490` (dry-run),
`cbe36b39` (rollout strategy fix — see the Deployment comment), `c4d9f920`
(writes on). Dry-run logged `fail-static:waiting-for-pool` at market
`0.005000` every poll, alerted once (ntfy id `uTw77kO4xbtK`), and created its
state ConfigMap. Step 3 waits for the pool.

New releases: bump `VERSION` in the commit that changes the code, push it,
and push tag `vX.Y.Z` on that commit while it is still main's tip — the tag
fires `warden-build-sensor` in iad-ci, which builds main's tip
(`release-version.py` takes `VERSION` when the tip raised it).

## Fire drill (repeatable)

Proves the defensive posture end to end on the real pool. Run only with writes
enabled and the guard `tracking` the real pool (desired 2, bid `0.015`). All
cluster reads use the credential-free read-only proxies; the only changes are
the two declarative-config commits below.

Work in a disposable clone of declarative-config (never the shared checkout):

```bash
D=$(mktemp -d ~/scratch/dc-drill-XXXX); trap 'rm -rf "$D"' EXIT
git clone -q --reference ~/declarative-config https://git.ardenone.com/jedarden/declarative-config.git "$D/dc"
cd "$D/dc" && git config user.name jedarden && git config user.email github@jedarden.com
F=k8s/rs-manager/warden/deployment.yaml
```

**0. Snapshot** every pool in the org (name, cloudspace, class, bid, desired,
`wonCount`) with a read-only `GET
/apis/ngpc.rxt.io/v1/namespaces/org-kselolwaoxl3zxfm/spotnodepools` (main-org
token by reference, never printed), and the iad-ci nodes:
`kubectl --server=http://traefik-iad-ci:8001 get nodes -L node.kubernetes.io/instance-type`.

**1. Drill commit — threshold below market** (market 0.005 → threshold 0.004):

```bash
cd "$D/dc" && sed -i 's/"offAbovePerHour":"0.012"/"offAbovePerHour":"0.004"/' $F && git diff --stat
cd "$D/dc" && git commit -m "drill(warden): spot guard threshold 0.004 for the fire drill (warden-faf60f86)" -- $F && git pull --no-rebase -q && git push -q origin main
```

Expect, within one poll of the restarted pod's first tick: log `action=off`,
alert **OFF** delivered (`ntfy_id` logged), the pool at `desired 0`, then
`wonCount 0` and the pool's nodes leaving iad-ci.

**2. Revert commit — the exact inverse of step 1:**

```bash
DRILL=$(cd "$D/dc" && git log -1 --format=%H --grep='spot guard threshold 0.004 for the fire drill')
cd "$D/dc" && git revert --no-edit "$DRILL" && git show --stat HEAD && git pull --no-rebase -q && git push -q origin main
```

The revert must restore `"offAbovePerHour":"0.012"` and change nothing else.
Expect: `condition=holding-off` with a growing `quiet_for`, **no** restore
before 30 minutes, then `action=restore`, alert **RESTORED**, the pool at
`desired 2`, Spot fulfils the `0.015` bid again (`bidStatus Fulfilled`,
`wonCount 2`), and the pool's nodes rejoin iad-ci `Ready`.

**3. Snapshot again** and diff against step 0: only the guarded pool's
`desired`/`wonCount` may have moved, and both are back at 2.

Record on the bead: both commit SHAs and times, the OFF and RESTORED `ntfy_id`s,
the guarded pool's desired/won before, during and after, the node names that
left and rejoined, and the step 0 / step 3 snapshot diff. Ask the human to
confirm both notifications arrived.
