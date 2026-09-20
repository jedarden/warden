package policy

import (
	"math/big"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

// GrandfatheredBid blocks every Warden write to an existing pool whose bid is
// below the server class's currently published minimum for new bids. Scaling
// even without changing bidPrice can release the old capacity, so there is no
// safe count-only exception. Unparseable or missing prices fail closed.
func GrandfatheredBid(pool spot.NodePool, class spot.ServerClass) Decision {
	if pool.Spec.ServerClass != class.Metadata.Name {
		return deny("server class response does not match pool (fail closed)")
	}
	bid, ok := positivePrice(pool.Spec.BidPrice)
	if !ok {
		return deny("cannot parse pool bidPrice %q (fail closed)", pool.Spec.BidPrice)
	}
	minimum, ok := positivePrice(class.Spec.MinBidPricePerHour)
	if !ok {
		return deny("server class minimum bid is missing or invalid (fail closed)")
	}
	if bid.Cmp(minimum) < 0 {
		return deny("pool bid %s is below current new-bid minimum %s; grandfathered pool is immutable through warden",
			pool.Spec.BidPrice, class.Spec.MinBidPricePerHour)
	}
	return allow("pool bid meets current new-bid minimum %s", class.Spec.MinBidPricePerHour)
}

func positivePrice(s string) (*big.Rat, bool) {
	v, ok := new(big.Rat).SetString(s)
	return v, ok && v.Sign() > 0
}
