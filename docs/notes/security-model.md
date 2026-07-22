# Security model

## Threat being addressed

Autonomous agents (NEEDLE workers, or an autoscaler they influence) need to
resize Spot node pools. The Rackspace Spot control-plane API is **org-scoped**
with coarse IAM — a token that can scale a pool can also delete cloudspaces and
reshape every cluster in the org. We must expose *only* bounded scaling, to
callers we don't fully trust, without ever handing them that token.

## Layered controls (floor → ceiling)

1. **Dedicated Spot org (floor).** The token warden holds belongs to an org that
   contains *only* worker cloudspaces. Worst case if warden is wholly bypassed
   or buggy: the worker fleet is damaged; production trading infra is untouched
   because it isn't in this token's org.

2. **Credential separation (primary control).** The Spot refresh token lives
   only inside warden (env from a SealedSecret). Agents never possess it. On
   bare-metal agent hosts (EX44, lab) there is no NetworkPolicy to rely on, so
   this — not the network — is what prevents agents calling Spot directly: they
   simply don't have a Spot credential.

3. **Intent API, not passthrough (fail closed by construction).** warden does
   not forward arbitrary caller requests. It exposes `list` and
   `scale(count)` only, reads the target pool, validates, then builds its *own*
   patch touching only the count. Create, delete, change-class, and change-bid
   have no endpoint and no field — they cannot be expressed, so they cannot leak
   through a policy bug.

4. **Invariant policy (ceiling).** Class allowlist, bid cap, and an org-wide
   node-ceiling that counts each pool's autoscaling max. Read-before-write so
   the org total is evaluated against the live set of pools. See
   `invariant-policy.md`.

5. **Network isolation (defense in depth).** warden runs in rs-manager and is
   reachable only over the cluster's single Tailscale/Traefik ingress — never
   the public internet.

6. **Audit.** Every decision, allow or deny, is logged with the caller
   fingerprint, action, pool, count, and reason.

## Caller authentication

- Callers present `Authorization: Bearer <token>`.
- Tokens are matched in constant time against sha256 digests of the configured
  allowlist. The raw token is never logged; audit records a 12-char digest
  fingerprint.
- This is a **shared-secret** stopgap. Per-agent identity, attribution, and
  revocation arrive when SEAM (Phase 7) and NEEDLE tsnet identity land — at
  which point warden's auth is replaced, not extended.

## Non-goals (deliberate)

- warden does not create or delete pools/cloudspaces. Pools are provisioned
  out-of-band (Terraform/console) in the dedicated org with the correct class
  and bid; warden only scales them within the envelope.
- warden does not manage the Spot org itself, billing, or bids.

## Absorption into SEAM

warden is the interim standalone form of a SEAM route. The intent API is the
future SEAM contract; the policy engine lifts into a SEAM route fragment; the
token moves behind SEAM. Nothing here is throwaway.
