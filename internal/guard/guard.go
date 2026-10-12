// Package guard is warden's internal Spot price guard. It watches one server
// class's market price and protects exactly one fixed pool in one
// organization: above the threshold it turns that pool off; once the price
// has stayed at or below the threshold continuously for RestoreAfter, it
// restores the pool — but only an off it made itself, and only while the
// pool is still exactly as it left it. Anything it cannot read or cannot
// interpret makes it change nothing and alert ("fail static").
//
// Its write authority is a Permit (permit.go), not the caller policy
// envelope: the guarded organization is read-only for every caller.
package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"git.ardenone.com/jedarden/warden/internal/config"
	"git.ardenone.com/jedarden/warden/internal/spot"
)

// Reader is the read half of the Spot API the guard uses.
type Reader interface {
	GetServerClass(ctx context.Context, name string) (*spot.ServerClass, error)
	ListNodePools(ctx context.Context, ns string) ([]spot.NodePool, error)
}

// Config is the guard's behaviour. See config.Guard for the env contract.
type Config struct {
	Namespace     string
	CloudSpace    string
	ServerClass   string
	OffAboveNanos int64
	OffAboveText  string
	RestoreAfter  time.Duration
	RestoreCount  int
	PollInterval  time.Duration
	DryRun        bool
}

// Conditions. A change of condition is alerted; actions are always alerted.
const (
	CondTracking        = "tracking"
	CondHolding         = "holding-off"
	CondDisarmed        = "hands-off"
	CondDryRunWouldOff  = "dry-run:would-off"
	CondDryRunWouldRest = "dry-run:would-restore"
	CondWaitingForPool  = "fail-static:waiting-for-pool"
	CondAmbiguous       = "fail-static:ambiguous"
	CondPoolShape       = "fail-static:pool-shape"
	CondPriceUnreadable = "fail-static:price-unreadable"
	CondPoolsUnreadable = "fail-static:pools-unreadable"
	CondStateUnreadable = "fail-static:state-unreadable"
	CondStateUnwritable = "fail-static:state-unwritable"
	CondWriteFailed     = "fail-static:write-failed"
	CondWriteBlocked    = "fail-static:write-blocked"
)

// Actions reported in Result.
const (
	ActOff          = "off"
	ActRestore      = "restore"
	ActRelease      = "release"
	ActWouldOff     = "would-off"
	ActWouldRestore = "would-restore"
	ActOffPending   = "off-unconfirmed"
	ActRestPending  = "restore-unconfirmed"
	ActReconciled   = "reconciled"
	ActWriteAborted = "write-not-applied"
)

// Result summarises one tick for logs and tests.
type Result struct {
	Condition string
	Action    string
	Detail    string
	Pool      string
	Price     string
}

// Guard is the state machine. Tick is not safe for concurrent use; Run calls
// it from one goroutine.
type Guard struct {
	cfg    Config
	spot   Reader
	permit *Permit
	store  Store
	alert  Alerter
	log    *slog.Logger
	now    func() time.Time

	state         *State // nil until loaded
	lastCondition string // survives an unreadable store
}

// New builds a guard. permit may wrap a nil writer in dry-run mode.
func New(cfg Config, r Reader, permit *Permit, store Store, alerter Alerter, log *slog.Logger, now func() time.Time) *Guard {
	if now == nil {
		now = time.Now
	}
	return &Guard{cfg: cfg, spot: r, permit: permit, store: store, alert: alerter, log: log, now: now}
}

// Run ticks every PollInterval until ctx is cancelled.
func (g *Guard) Run(ctx context.Context) {
	g.log.Info("spot guard starting", "namespace", g.cfg.Namespace, "cloudspace", g.cfg.CloudSpace,
		"server_class", g.cfg.ServerClass, "off_above", g.cfg.OffAboveText, "restore_after", g.cfg.RestoreAfter.String(),
		"restore_count", g.cfg.RestoreCount, "poll", g.cfg.PollInterval.String(), "dry_run", g.cfg.DryRun)
	t := time.NewTicker(g.cfg.PollInterval)
	defer t.Stop()
	for {
		tctx, cancel := context.WithTimeout(ctx, g.cfg.PollInterval)
		g.Tick(tctx)
		cancel()
		select {
		case <-ctx.Done():
			g.log.Info("spot guard stopped")
			return
		case <-t.C:
		}
	}
}

// Tick runs one observe-decide-act cycle and logs its outcome.
func (g *Guard) Tick(ctx context.Context) Result {
	r := g.tick(ctx)
	attrs := []any{"dry_run", g.cfg.DryRun, "condition", r.Condition, "action", r.Action,
		"pool", r.Pool, "market", r.Price, "off_above", g.cfg.OffAboveText, "detail", r.Detail}
	if st := g.state; st != nil {
		attrs = append(attrs, "armed", st.Armed)
		if st.Owned != nil {
			attrs = append(attrs, "owned_phase", st.Owned.Phase, "owned_pool", st.Owned.Pool, "restore_to", st.Owned.RestoreTo)
		}
		if st.QuietSince != nil {
			attrs = append(attrs, "quiet_for", g.now().Sub(*st.QuietSince).Round(time.Second).String())
		}
	}
	g.log.Info("spot guard tick", attrs...)
	return r
}

func (g *Guard) maxGap() time.Duration { return 2*g.cfg.PollInterval + g.cfg.PollInterval/2 }

func (g *Guard) tick(ctx context.Context) Result {
	now := g.now()
	if g.state == nil {
		st, err := g.store.Load(ctx)
		if err != nil {
			return g.failStatic(ctx, CondStateUnreadable, "cannot read guard state: "+err.Error(), Result{}, false)
		}
		if st == nil {
			st = NewState()
		}
		g.state = st
		g.lastCondition = st.Condition
	}
	st := g.state
	prevHigh, prevAt := st.LastPriceHigh, st.LastPriceAt

	price, priceText, perr := g.readPrice(ctx)
	res := Result{Price: priceText}
	if st.WriteBlocked != nil {
		// Monitor without letting price, identity or spec changes re-arm writes.
		// An existing confirmed off remains owned until an operator resets the
		// latch; clearing ownership here could strand that pool at zero.
		res.Pool = st.WriteBlocked.Pool
		detail := "protection unavailable; writes blocked until explicit operator reset: " + st.WriteBlocked.Reason
		if perr != nil {
			detail += "; price monitoring failed: " + perr.Error()
		}
		if _, err := g.spot.ListNodePools(ctx, g.cfg.Namespace); err != nil {
			detail += "; pool monitoring failed: " + err.Error()
		}
		// No restore continuity can be inferred through a blocked interval.
		st.QuietSince, st.LastPriceAt = nil, nil
		return g.failStatic(ctx, CondWriteBlocked, detail, res, true)
	}
	if perr != nil {
		// Continuity of "at or below" cannot be proven across an unreadable
		// price: the restore timer starts over.
		st.QuietSince = nil
		return g.failStatic(ctx, CondPriceUnreadable, perr.Error(), res, true)
	}
	high := price > g.cfg.OffAboveNanos
	continuous := prevAt != nil && now.Sub(*prevAt) <= g.maxGap()
	if st.Owned != nil {
		switch {
		case high:
			if st.QuietSince != nil {
				g.log.Info("spot guard: spike resets restore timer", "market", priceText, "off_above", g.cfg.OffAboveText)
			}
			st.QuietSince = nil
		case st.QuietSince == nil || !continuous:
			t := now
			st.QuietSince = &t
		}
	}
	if !high {
		st.Armed = true
	}
	t := now
	st.LastPriceAt, st.LastPriceHigh = &t, high

	pools, err := g.spot.ListNodePools(ctx, g.cfg.Namespace)
	if err != nil {
		return g.failStatic(ctx, CondPoolsUnreadable, "cannot list pools: "+err.Error(), res, true)
	}
	if st.Owned != nil {
		return g.tickOwned(ctx, now, pools, high, res)
	}
	return g.tickArmed(ctx, now, pools, high, prevHigh && continuous, res)
}

func (g *Guard) readPrice(ctx context.Context) (int64, string, error) {
	class, err := g.spot.GetServerClass(ctx, g.cfg.ServerClass)
	if err != nil {
		return 0, "", fmt.Errorf("cannot read server class %s: %v", g.cfg.ServerClass, err)
	}
	text := class.Status.SpotPricing.MarketPricePerHour
	n, err := config.ParseDecimalNanos(text)
	if err != nil {
		return 0, text, fmt.Errorf("server class %s marketPricePerHour unreadable: %v", g.cfg.ServerClass, err)
	}
	return n, text, nil
}

func (g *Guard) tickArmed(ctx context.Context, now time.Time, pools []spot.NodePool, high, wasHighContinuously bool, res Result) Result {
	st := g.state
	matches := Matches(pools, g.cfg.CloudSpace, g.cfg.ServerClass)
	switch len(matches) {
	case 0:
		return g.failStatic(ctx, CondWaitingForPool, fmt.Sprintf("no pool in cloudspace %s with class %s (waiting for pool)", g.cfg.CloudSpace, g.cfg.ServerClass), res, true)
	case 1:
	default:
		return g.failStatic(ctx, CondAmbiguous, fmt.Sprintf("%d pools match cloudspace %s class %s: %s", len(matches), g.cfg.CloudSpace, g.cfg.ServerClass, poolNames(matches)), res, true)
	}
	p := matches[0]
	res.Pool = p.Metadata.Name
	if p.Autoscaled() || p.Spec.Desired == nil || *p.Spec.Desired < 0 || p.Metadata.ResourceVersion == "" {
		return g.failStatic(ctx, CondPoolShape, fmt.Sprintf("pool %s is not a well-formed fixed pool with a resourceVersion (autoscaled=%t)", p.Metadata.Name, p.Autoscaled()), res, true)
	}
	desired := *p.Spec.Desired
	g.watchFulfillment(ctx, p)

	// Someone else changed the pool's spec while the price stayed above the
	// threshold: that is a deliberate decision made during the spike, so the
	// guard keeps its hands off until the price is back at or below.
	if high && wasHighContinuously && st.Armed && st.Seen != nil && st.Seen.UID == p.Metadata.UID &&
		st.Seen.Generation != p.Metadata.Generation {
		st.Armed = false
		g.log.Info("spot guard: pool changed by someone else during a spike; hands off until price returns", "pool", p.Metadata.Name)
	}
	st.Seen = &Seen{Pool: p.Metadata.Name, UID: p.Metadata.UID, Generation: p.Metadata.Generation}

	if high && st.Armed && desired > 0 {
		if g.cfg.DryRun {
			res.Action = ActWouldOff
			res.Detail = fmt.Sprintf("would scale %s %d→0", p.Metadata.Name, desired)
			g.transition(ctx, CondDryRunWouldOff, g.alertf("high", []string{"warning"},
				"%s: would turn pool OFF (market %s > %s)", "Would scale pool %s %d→0. Market %s/h is above %s/h. Dry-run: nothing was changed.\n%s",
				g.scope(), res.Price, g.cfg.OffAboveText, p.Metadata.Name, desired, res.Price, g.cfg.OffAboveText, describe(p)))
			res.Condition = CondDryRunWouldOff
			g.save(ctx)
			return res
		}
		return g.turnOff(ctx, now, pools, p, res)
	}

	cond, detail := CondTracking, fmt.Sprintf("pool %s desired %d; market %s/h, off above %s/h", p.Metadata.Name, desired, res.Price, g.cfg.OffAboveText)
	if high && !st.Armed {
		cond = CondDisarmed
		detail = "price above threshold but the pool was changed by someone else during this spike (or after the guard's off); hands off until the price is at or below the threshold"
	}
	if high && desired == 0 && st.Armed {
		detail = fmt.Sprintf("price above threshold; pool %s already at 0 (not the guard's off, nothing to restore)", p.Metadata.Name)
	}
	g.transition(ctx, cond, g.conditionAlert(cond, detail, p))
	res.Condition, res.Detail = cond, detail
	g.save(ctx)
	return res
}

func (g *Guard) turnOff(ctx context.Context, now time.Time, pools []spot.NodePool, p spot.NodePool, res Result) Result {
	st := g.state
	desired := *p.Spec.Desired
	// A guard restore always sets restoreCount, whatever the pool held before
	// the off (user decision 2026-10-11: "Let it set 2. If rackspace spot can
	// only provide 1 then the second node just stays inaccessible.").
	restoreTo := g.cfg.RestoreCount
	st.Owned = &Owned{Pool: p.Metadata.Name, UID: p.Metadata.UID, CloudSpace: p.Spec.CloudSpace, ServerClass: p.Spec.ServerClass,
		BidPrice: p.Spec.BidPrice, PreOffCount: desired, PreOffGeneration: p.Metadata.Generation,
		RestoreTo: restoreTo, OffAt: now, Phase: PhaseOffPending}
	st.Armed = false
	st.QuietSince = nil
	st.Fulfillment = nil
	// Intent is durable before the write, so a crash mid-write is reconciled
	// from the pool's generation rather than forgotten.
	if err := g.persist(ctx); err != nil {
		st.Owned, st.Armed = nil, true
		return g.failStatic(ctx, CondStateUnwritable, "cannot persist off intent; not writing: "+err.Error(), res, false)
	}
	resp, err := g.permit.Scale(ctx, pools, p.Metadata.Name, 0)
	switch {
	case err == nil && resp != nil && landedOff(st.Owned, *resp):
		return g.confirmOff(ctx, *resp, res, "")
	case err == nil:
		res.Action, res.Detail = ActOffPending, "off write accepted but not confirmed; reconciling next tick"
		res.Condition = g.lastCondition
		g.save(ctx)
		return res
	case spot.IsWriteRejected(err):
		// An explicit refusal is not an uncertain off and confers no restore
		// ownership. Persist the block instead of re-deciding every tick.
		st.Owned = nil
		return g.blockWrites(ctx, p.Metadata.Name, "off write rejected: "+err.Error(), res)
	case definitive(err):
		st.Owned, st.Armed = nil, true
		res.Action = ActWriteAborted
		return g.failStatic(ctx, CondWriteFailed, "off write not applied: "+err.Error(), res, true)
	default:
		res.Action = ActOffPending
		return g.failStatic(ctx, CondWriteFailed, "off write outcome unknown (reconciling next tick): "+err.Error(), res, true)
	}
}

func (g *Guard) confirmOff(ctx context.Context, p spot.NodePool, res Result, note string) Result {
	st := g.state
	o := st.Owned
	o.OffGeneration = p.Metadata.Generation
	o.Phase = PhaseOff
	st.Seen = &Seen{Pool: p.Metadata.Name, UID: p.Metadata.UID, Generation: p.Metadata.Generation}
	restore := fmt.Sprintf("Will restore to %d after %s continuously at or below %s/h.", o.RestoreTo, g.cfg.RestoreAfter, g.cfg.OffAboveText)
	g.act(ctx, CondHolding, g.alertf("high", []string{"rotating_light"},
		"%s: pool turned OFF (market %s > %s)", "Scaled pool %s %d→0 because market %s/h is above %s/h.%s %s\n%s",
		g.scope(), res.Price, g.cfg.OffAboveText, o.Pool, o.PreOffCount, res.Price, g.cfg.OffAboveText, note, restore, describe(p)))
	g.save(ctx)
	res.Action, res.Condition, res.Pool = ActOff, CondHolding, o.Pool
	res.Detail = fmt.Sprintf("scaled %s %d→0", o.Pool, o.PreOffCount)
	return res
}

func (g *Guard) tickOwned(ctx context.Context, now time.Time, pools []spot.NodePool, high bool, res Result) Result {
	st := g.state
	o := st.Owned
	res.Pool = o.Pool
	var p *spot.NodePool
	for i := range pools {
		if pools[i].Metadata.Name == o.Pool {
			p = &pools[i]
			break
		}
	}
	if p == nil {
		return g.release(ctx, nil, high, res, fmt.Sprintf("pool %s no longer exists", o.Pool))
	}
	if o.UID != "" && p.Metadata.UID != o.UID {
		return g.release(ctx, p, high, res, fmt.Sprintf("pool %s was replaced (uid changed)", o.Pool))
	}

	switch o.Phase {
	case PhaseOffPending:
		switch {
		case landedOff(o, *p):
			return g.confirmOff(ctx, *p, res, " (Confirmed on reconcile.)")
		case unchangedSincePreOff(o, *p):
			// The off never landed. Forget the claim and decide afresh.
			g.log.Info("spot guard: off write did not land; re-deciding", "pool", o.Pool)
			st.Owned, st.Armed = nil, true
			res.Action = ActReconciled
			r := g.tickArmed(ctx, now, pools, high, false, res)
			if r.Action == "" {
				r.Action = ActReconciled
			}
			return r
		default:
			return g.release(ctx, p, high, res, "pool changed by someone else while the guard's off was unconfirmed: "+diffFrom(o, *p, 0, o.PreOffGeneration+1))
		}
	case PhaseRestorePending:
		switch {
		case landedRestore(o, *p):
			return g.finishRestore(ctx, *p, res, " (Confirmed on reconcile.)")
		case diffFrom(o, *p, 0, o.OffGeneration) == "":
			g.log.Info("spot guard: restore write did not land; will retry", "pool", o.Pool)
			o.Phase = PhaseOff
		default:
			return g.release(ctx, p, high, res, "pool changed by someone else while the guard's restore was unconfirmed: "+diffFrom(o, *p, o.RestoreTo, o.OffGeneration+1))
		}
	}

	// PhaseOff: restore only an off that is still exactly as the guard left it.
	if d := diffFrom(o, *p, 0, o.OffGeneration); d != "" {
		return g.release(ctx, p, high, res, "pool changed by someone else after the guard's off: "+d)
	}
	quietFor := time.Duration(0)
	if st.QuietSince != nil {
		quietFor = now.Sub(*st.QuietSince)
	}
	if st.QuietSince == nil || quietFor < g.cfg.RestoreAfter {
		detail := fmt.Sprintf("holding %s at 0; at or below %s/h for %s of %s", o.Pool, g.cfg.OffAboveText, quietFor.Round(time.Second), g.cfg.RestoreAfter)
		if high {
			detail = fmt.Sprintf("holding %s at 0; market %s/h above %s/h", o.Pool, res.Price, g.cfg.OffAboveText)
		}
		g.transition(ctx, CondHolding, g.conditionAlert(CondHolding, detail, *p))
		res.Condition, res.Detail = CondHolding, detail
		g.save(ctx)
		return res
	}
	// Claims persisted by an older build may carry another target; a guard
	// restore is always restoreCount.
	o.RestoreTo = g.cfg.RestoreCount
	if n := len(Matches(pools, g.cfg.CloudSpace, g.cfg.ServerClass)); n != 1 {
		return g.failStatic(ctx, CondAmbiguous, fmt.Sprintf("restore due but %d pools match cloudspace %s class %s; not writing", n, g.cfg.CloudSpace, g.cfg.ServerClass), res, true)
	}
	if g.cfg.DryRun {
		// Only reachable when writes were switched off while an off was
		// outstanding: report, keep the claim, write nothing.
		res.Action, res.Condition = ActWouldRestore, CondDryRunWouldRest
		res.Detail = fmt.Sprintf("would scale %s 0→%d", o.Pool, o.RestoreTo)
		g.transition(ctx, CondDryRunWouldRest, g.alertf("default", []string{"hourglass"},
			"%s: would RESTORE pool to %d", "%s\n%s", g.scope(), o.RestoreTo, res.Detail, describe(*p)))
		g.save(ctx)
		return res
	}
	return g.restore(ctx, pools, *p, res)
}

func (g *Guard) restore(ctx context.Context, pools []spot.NodePool, p spot.NodePool, res Result) Result {
	o := g.state.Owned
	o.Phase = PhaseRestorePending
	if err := g.persist(ctx); err != nil {
		o.Phase = PhaseOff
		return g.failStatic(ctx, CondStateUnwritable, "cannot persist restore intent; not writing: "+err.Error(), res, false)
	}
	resp, err := g.permit.Scale(ctx, pools, o.Pool, o.RestoreTo)
	switch {
	case err == nil && resp != nil && landedRestore(o, *resp):
		return g.finishRestore(ctx, *resp, res, "")
	case err == nil:
		res.Action, res.Detail, res.Condition = ActRestPending, "restore write accepted but not confirmed; reconciling next tick", g.lastCondition
		g.save(ctx)
		return res
	case spot.IsWriteRejected(err):
		// The off remains ours; only the rejected restore intent is dropped.
		o.Phase = PhaseOff
		return g.blockWrites(ctx, o.Pool, "restore write rejected: "+err.Error(), res)
	case definitive(err):
		o.Phase = PhaseOff
		res.Action = ActWriteAborted
		return g.failStatic(ctx, CondWriteFailed, "restore write not applied: "+err.Error(), res, true)
	default:
		res.Action = ActRestPending
		return g.failStatic(ctx, CondWriteFailed, "restore write outcome unknown (reconciling next tick): "+err.Error(), res, true)
	}
}

func (g *Guard) finishRestore(ctx context.Context, p spot.NodePool, res Result, note string) Result {
	st := g.state
	o := st.Owned
	st.Owned, st.QuietSince, st.Armed = nil, nil, true
	st.Seen = &Seen{Pool: p.Metadata.Name, UID: p.Metadata.UID, Generation: p.Metadata.Generation}
	won := wonOf(p)
	st.Fulfillment = &FulfillmentWatch{Pool: p.Metadata.Name, UID: p.Metadata.UID, Reported: won}
	g.act(ctx, CondTracking, g.alertf("default", []string{"white_check_mark"},
		"%s: pool RESTORED: desired %d, fulfilled %s", "Scaled pool %s 0→%d: market %s/h has been at or below %s/h for %s since the guard's off at %s.%s Spot fulfils the bid on its own schedule; the guard does not re-issue the scale and sends one follow-up if fulfilment changes.\n%s",
		g.scope(), o.RestoreTo, countText(won), o.Pool, o.RestoreTo, res.Price, g.cfg.OffAboveText, g.cfg.RestoreAfter, o.OffAt.UTC().Format(time.RFC3339), note, describe(p)))
	g.save(ctx)
	res.Action, res.Condition, res.Pool = ActRestore, CondTracking, o.Pool
	res.Detail = fmt.Sprintf("scaled %s 0→%d", o.Pool, o.RestoreTo)
	return res
}

// fulfillmentSettlePolls is how many consecutive polls a changed fulfilment
// must hold before the single post-restore follow-up is sent, so a pool
// filling 0→1→2 reports once, at its settled value.
const fulfillmentSettlePolls = 5

// watchFulfillment sends at most one follow-up after a guard restore, when
// status.wonCount has settled at a value other than the one the RESTORED alert
// reported. It is reporting only: fulfilment never feeds a decision, never
// counts as anyone's change, and never causes a write (Spot filling fewer
// nodes than desired is the market, not a human).
func (g *Guard) watchFulfillment(ctx context.Context, p spot.NodePool) {
	st := g.state
	w := st.Fulfillment
	if w == nil {
		return
	}
	if p.Metadata.Name != w.Pool || (w.UID != "" && p.Metadata.UID != w.UID) {
		st.Fulfillment = nil
		return
	}
	won := wonOf(p)
	if won == nil || (w.Reported != nil && *won == *w.Reported) {
		w.Candidate, w.StablePolls = nil, 0
		return
	}
	if w.Candidate != nil && *w.Candidate == *won {
		w.StablePolls++
	} else {
		w.Candidate, w.StablePolls = won, 1
	}
	if w.StablePolls < fulfillmentSettlePolls {
		return
	}
	st.Fulfillment = nil
	desired := 0
	if p.Spec.Desired != nil {
		desired = *p.Spec.Desired
	}
	g.act(ctx, g.lastCondition, g.alertf("low", []string{"information_source"},
		"%s: fulfilment now %d of %d", "Since the guard's restore Spot fulfilment changed from %s to %d (desired %d). No action taken; this is the only follow-up.\n%s",
		g.scope(), *won, desired, countText(w.Reported), *won, desired, describe(p)))
}

func wonOf(p spot.NodePool) *int {
	if p.Status.WonCount == nil {
		return nil
	}
	n := *p.Status.WonCount
	return &n
}

func countText(n *int) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprint(*n)
}

// release drops the guard's claim without writing anything. Armed stays off
// while the price is still high, so the guard never fights the change that
// caused the release; p (nil if the pool is gone) becomes the last-seen pool.
func (g *Guard) release(ctx context.Context, p *spot.NodePool, high bool, res Result, reason string) Result {
	st := g.state
	st.Owned, st.QuietSince = nil, nil
	st.Armed = !high
	st.Seen = nil
	if p != nil {
		st.Seen = &Seen{Pool: p.Metadata.Name, UID: p.Metadata.UID, Generation: p.Metadata.Generation}
	}
	cond := CondTracking
	if high {
		cond = CondDisarmed
	}
	g.act(ctx, cond, g.alertf("default", []string{"raised_hand"},
		"%s: guard released the pool (no restore)", "%s. The guard will not restore or change it.",
		g.scope(), reason))
	g.save(ctx)
	res.Action, res.Condition, res.Detail = ActRelease, cond, reason
	return res
}

func (g *Guard) failStatic(ctx context.Context, cond, detail string, res Result, persist bool) Result {
	g.transition(ctx, cond, g.alertf("high", []string{"warning"},
		"%s: FAIL-STATIC (%s)", "No change made. %s", g.scope(), strings.TrimPrefix(cond, "fail-static:"), detail))
	if persist {
		g.save(ctx)
	}
	res.Condition, res.Detail = cond, detail
	return res
}

func (g *Guard) blockWrites(ctx context.Context, pool, reason string, res Result) Result {
	g.state.WriteBlocked = &WriteBlock{Pool: pool, Reason: reason, At: g.now().UTC()}
	g.state.Armed = false
	g.state.QuietSince, g.state.LastPriceAt = nil, nil
	// Persist before alert delivery, which can time out and exhaust the tick's
	// context. A paging failure must not lose the rejected-write latch.
	g.state.Condition = CondWriteBlocked
	detail := "protection unavailable; writes blocked until explicit operator reset: " + reason
	if err := g.persist(ctx); err != nil {
		detail += "; cannot persist write block (restart durability unavailable): " + err.Error()
	}
	res.Action = ActWriteAborted
	return g.failStatic(ctx, CondWriteBlocked, detail, res, false)
}

// transition alerts only when the condition changes.
func (g *Guard) transition(ctx context.Context, cond string, a Alert) {
	if cond == g.lastCondition {
		return
	}
	g.act(ctx, cond, a)
}

// act always alerts and records the resulting condition.
func (g *Guard) act(ctx context.Context, cond string, a Alert) {
	g.lastCondition = cond
	if g.state != nil {
		g.state.Condition = cond
	}
	id, err := g.alert.Send(ctx, a)
	if err != nil {
		g.log.Error("spot guard alert delivery failed", "condition", cond, "title", a.Title, "err", err)
		return
	}
	g.log.Info("spot guard alert delivered", "condition", cond, "title", a.Title, "ntfy_id", id)
}

func (g *Guard) conditionAlert(cond, detail string, p spot.NodePool) Alert {
	switch cond {
	case CondHolding:
		return g.alertf("default", []string{"hourglass"}, "%s: holding pool off", "%s\n%s", g.scope(), detail, describe(p))
	case CondDisarmed:
		return g.alertf("default", []string{"raised_hand"}, "%s: hands off for this spike", "%s\n%s", g.scope(), detail, describe(p))
	default:
		return g.alertf("low", []string{"eyes"}, "%s: tracking pool %s", "%s\n%s", g.scope(), p.Metadata.Name, detail, describe(p))
	}
}

func (g *Guard) scope() string {
	return fmt.Sprintf("warden spot guard %s/%s", g.cfg.CloudSpace, g.cfg.ServerClass)
}

// alertf formats title and body from one argument list: the title consumes
// as many leading args as it has verbs.
func (g *Guard) alertf(prio string, tags []string, title, body string, args ...any) Alert {
	n := strings.Count(title, "%") - 2*strings.Count(title, "%%")
	t := fmt.Sprintf(title, args[:n]...)
	b := fmt.Sprintf(body, args[n:]...)
	if g.cfg.DryRun {
		t = "[DRY-RUN] " + t
		b = "[DRY-RUN: this guard never writes]\n" + b
	}
	return Alert{Title: t, Body: b, Priority: prio, Tags: tags}
}

func (g *Guard) persist(ctx context.Context) error {
	g.state.Version = StateVersion
	g.state.UpdatedAt = g.now().UTC()
	err := g.store.Save(ctx, g.state)
	if errors.Is(err, ErrStateConflict) {
		// Someone else wrote the state: reload before acting again, except that
		// a known upstream rejection must keep this process blocked even if the
		// latch could not be saved. An operator reset requires a restart.
		if g.state.WriteBlocked == nil {
			g.state = nil
		}
	}
	return err
}

func (g *Guard) save(ctx context.Context) {
	if g.state == nil {
		return
	}
	if err := g.persist(ctx); err != nil {
		g.log.Error("spot guard: cannot persist state", "err", err)
	}
}

func definitive(err error) bool {
	return errors.Is(err, ErrNotPermitted) || spot.IsConflict(err) || spot.IsPreconditionRejected(err) || errors.Is(err, spot.ErrNotFound)
}

func sameShape(o *Owned, p spot.NodePool) bool {
	return p.Spec.CloudSpace == o.CloudSpace && p.Spec.ServerClass == o.ServerClass && p.Spec.BidPrice == o.BidPrice && !p.Autoscaled() &&
		p.Spec.Desired != nil && (o.UID == "" || p.Metadata.UID == o.UID)
}

func landedOff(o *Owned, p spot.NodePool) bool {
	return sameShape(o, p) && *p.Spec.Desired == 0 && (o.PreOffGeneration == 0 || p.Metadata.Generation == o.PreOffGeneration+1)
}

func unchangedSincePreOff(o *Owned, p spot.NodePool) bool {
	return sameShape(o, p) && *p.Spec.Desired == o.PreOffCount && p.Metadata.Generation == o.PreOffGeneration
}

func landedRestore(o *Owned, p spot.NodePool) bool {
	return sameShape(o, p) && *p.Spec.Desired == o.RestoreTo && (o.OffGeneration == 0 || p.Metadata.Generation == o.OffGeneration+1)
}

// diffFrom describes how p differs from the owned pool at the given count and
// generation; "" means identical.
func diffFrom(o *Owned, p spot.NodePool, count int, generation int64) string {
	var d []string
	if p.Spec.CloudSpace != o.CloudSpace {
		d = append(d, fmt.Sprintf("cloudspace %q→%q", o.CloudSpace, p.Spec.CloudSpace))
	}
	if p.Spec.ServerClass != o.ServerClass {
		d = append(d, fmt.Sprintf("class %q→%q", o.ServerClass, p.Spec.ServerClass))
	}
	if p.Spec.BidPrice != o.BidPrice {
		d = append(d, fmt.Sprintf("bid %q→%q", o.BidPrice, p.Spec.BidPrice))
	}
	if p.Autoscaled() {
		d = append(d, "autoscaling enabled")
	}
	switch {
	case p.Spec.Desired == nil:
		d = append(d, "desired missing")
	case *p.Spec.Desired != count:
		d = append(d, fmt.Sprintf("desired %d (expected %d)", *p.Spec.Desired, count))
	}
	if generation != 0 && p.Metadata.Generation != generation {
		d = append(d, fmt.Sprintf("generation %d (expected %d)", p.Metadata.Generation, generation))
	}
	return strings.Join(d, ", ")
}

func describe(p spot.NodePool) string {
	won := "?"
	if p.Status.WonCount != nil {
		won = fmt.Sprint(*p.Status.WonCount)
	}
	desired := "?"
	if p.Spec.Desired != nil {
		desired = fmt.Sprint(*p.Spec.Desired)
	}
	return fmt.Sprintf("pool=%s cloudspace=%s class=%s bid=%s desired=%s won=%s bidStatus=%s generation=%d",
		p.Metadata.Name, p.Spec.CloudSpace, p.Spec.ServerClass, p.Spec.BidPrice, desired, won, p.Status.BidStatus, p.Metadata.Generation)
}

func poolNames(ps []spot.NodePool) string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Metadata.Name)
	}
	return strings.Join(names, ", ")
}
