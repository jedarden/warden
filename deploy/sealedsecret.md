# Building the `warden-secrets` SealedSecret

warden reads three secret values from env. They are delivered via a SealedSecret
sealed to rs-manager's controller; **the plaintext never lands in git or on
disk in this repo.**

| Env key | Value |
|---------|-------|
| `WARDEN_SPOT_REFRESH_TOKEN` | Refresh token from the **dedicated** Spot org's console → API Access → Terraform |
| `WARDEN_ORG_NAMESPACE` | The dedicated org namespace, `org-<id>` (from `GET /apis/auth.ngpc.rxt.io/v1/organizations`) |
| `WARDEN_CALLER_TOKENS` | Comma-separated bearer token(s) for callers (autoscaler / dispatcher). Generate with `openssl rand -hex 32`. |

## Create it

```bash
# 1. Build a plain Secret locally (do NOT commit this file)
kubectl create secret generic warden-secrets \
  --namespace warden \
  --from-literal=WARDEN_SPOT_REFRESH_TOKEN="$SPOT_REFRESH_TOKEN" \
  --from-literal=WARDEN_ORG_NAMESPACE="org-xxxxxxxx" \
  --from-literal=WARDEN_CALLER_TOKENS="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml > /tmp/warden-secret.yaml

# 2. Seal it to rs-manager's controller
kubeseal --format yaml \
  --controller-namespace kube-system \
  --controller-name sealed-secrets \
  < /tmp/warden-secret.yaml > sealedsecret.yaml

# 3. Commit sealedsecret.yaml to declarative-config (k8s/rs-manager/warden/), shred the plaintext
shred -u /tmp/warden-secret.yaml
```

Give the generated caller token to the autoscaler/dispatcher out-of-band (its
own SealedSecret). Rotating a caller token = reseal + redeploy; rotating the
Spot refresh token = regenerate in the Spot console + reseal.
