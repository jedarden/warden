package policy

import (
	"strings"
	"testing"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

func TestGrandfatheredBid(t *testing.T) {
	for _, tc := range []struct {
		name, bid, minimum string
		allow              bool
	}{
		{"below new-bid floor", "0.001", "0.01", false},
		{"equal to floor", "0.01", "0.01", true},
		{"above floor", "0.02", "0.01", true},
		{"exact decimal comparison", "0.009999999999999999", "0.01", false},
		{"missing bid", "", "0.01", false},
		{"invalid bid", "NaN", "0.01", false},
		{"missing minimum", "0.01", "", false},
		{"invalid minimum", "0.01", "NaN", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := spot.NodePool{Spec: spot.NodePoolSpec{ServerClass: "class-a", BidPrice: tc.bid}}
			class := spot.ServerClass{Metadata: spot.Metadata{Name: "class-a"}}
			class.Spec.MinBidPricePerHour = tc.minimum
			d := GrandfatheredBid(pool, class)
			if d.Allow != tc.allow {
				t.Fatalf("decision=%+v", d)
			}
			if !tc.allow && !strings.Contains(d.Reason, "fail closed") && !strings.Contains(d.Reason, "grandfathered") {
				t.Fatalf("denial reason=%q", d.Reason)
			}
		})
	}
}
