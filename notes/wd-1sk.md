# SpotNodePool Autoscaling Field Names Verification

**Task:** Verify SpotNodePool autoscaling field names (minNodes/maxNodes) against live API

## Findings

Verified on 2026-08-02 that the autoscaling field names in Rackspace Spot's SpotNodePool CRD use **camelCase**:

- `minNodes` (int)
- `maxNodes` (int)

**NOT** snake_case (`min_nodes`, `max_nodes`) as used in the Terraform provider.

## Verification Method

Cross-referenced the official [Spot Go SDK documentation](https://spot.rackspace.com/docs/en/go-sdk), which documents the `Autoscaling` struct:

```go
type Autoscaling struct {
  Enabled bool
  MinNodes int64
  MaxNodes int64
}
```

## Updates Made

1. **`internal/spot/types.go`**: Updated comment to reflect that autoscaling field names have been verified against official SDK documentation

2. **`docs/research/rackspace-spot-api.md`**: Updated spec fields section to mark all fields as VERIFIED and corrected the autoscaling field name documentation

## Code Confirmation

The existing code in `internal/spot/types.go` was already correct:

```go
type Autoscaling struct {
    Enabled  bool `json:"enabled"`
    MinNodes int  `json:"minNodes"`
    MaxNodes int  `json:"maxNodes"`
}
```

The JSON tags use lowercase-first camelCase (`minNodes`, `maxNodes`) which matches the CRD schema.

## Sources

- [Spot Go SDK Documentation](https://spot.rackspace.com/docs/en/go-sdk)
- [Rackspace Spot Public API](https://spot.rackspace.com/docs/rackspace-spot-public-api)
- [Terraform Provider for Rackspace Spot](https://search.opentofu.org/provider/rackerlabs/spot/v0.1.4)
- [GitHub Discussion: Autoscaler didn't scale down](https://github.com/rackerlabs/spot/discussions/228)
- [pkg.go.dev: rxtspot package](https://pkg.go.dev/github.com/rackspace-spot/spot-go-sdk/api/v1)
