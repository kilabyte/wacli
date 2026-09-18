package app

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Reconnect flap governor tuning. A "flap" is a reconnect that fires again before
// the previous connection stayed up for flapStableAfter — i.e. the socket connects
// (noise handshake succeeds, ConnectContext returns nil) but the server drops it
// right back (classically a duplicate linked-device "ghost" session left behind by
// an unclean restart keeps triggering remote events.Disconnected). Without a governor
// this spins at websocket-handshake speed forever: a.reconnect -> ReconnectWithBackoff
// only backs off on connect *errors*, so a connect that succeeds-then-drops applies no
// delay, and maxReconnect never trips because each reconnect() returns nil fast.
const (
	flapBackoffMin = 1 * time.Second
	flapBackoffMax = 30 * time.Second
	// flapStableAfter: a (re)connection that holds at least this long is considered
	// recovered — it resets the backoff and the unstable-reconnect counter. Chosen above
	// the daemon's --stale-threshold (30s) and one keepalive cycle (<=30s) so a single
	// stale-driven reconnect that then holds normally does NOT count as a flap.
	flapStableAfter = 60 * time.Second
	// maxUnstableReconnects: give up (exit non-zero so a supervisor respawns us with a
	// clean slate, which re-auths a fresh socket and clears the server-side ghost session)
	// after this many CONSECUTIVE reconnects that each failed to hold flapStableAfter. With
	// the 30s backoff cap this is ~13 min of continuous flapping — far above ordinary
	// network blips, which reset the counter the moment one reconnect holds. Decoupled from
	// --max-reconnect (which remains the single-attempt deadline), so the two never
	// double-count.
	maxUnstableReconnects = 30
)

// flapSleep waits for d or until ctx is cancelled, reporting whether it slept the full
// duration. Indirected so tests can exercise the governor without real backoff sleeps.
var flapSleep = func(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// flapGovernor decides how long to back off before each reconnect, and when to give up
// entirely, so a connect-succeeds-then-server-drops loop can't spin without bound. It is
// not safe for concurrent use; runSyncFollow drives it from a single goroutine.
type flapGovernor struct {
	maxUnstable   int       // give up after this many consecutive unstable reconnects (0 = never)
	unstable      int       // consecutive reconnects that did not hold flapStableAfter
	backoff       time.Duration
	lastConnected time.Time // time of the last successful (re)connect
}

func newFlapGovernor(connectedAt time.Time, maxUnstable int) *flapGovernor {
	return &flapGovernor{maxUnstable: maxUnstable, lastConnected: connectedAt}
}

// beforeReconnect is called when a reconnect is requested at time now. It returns the
// backoff to wait first and whether the daemon should give up. A reconnect that arrives
// while the previous connection had held >= flapStableAfter is a fresh, isolated drop
// (no backoff, counter reset); one that arrives sooner is a flap (grow backoff, count it).
func (g *flapGovernor) beforeReconnect(now time.Time) (backoff time.Duration, giveUp bool) {
	if now.Sub(g.lastConnected) >= flapStableAfter {
		g.backoff = 0
		g.unstable = 0
	} else {
		g.unstable++
		switch {
		case g.backoff == 0:
			g.backoff = flapBackoffMin
		case g.backoff < flapBackoffMax:
			g.backoff *= 2
			if g.backoff > flapBackoffMax {
				g.backoff = flapBackoffMax
			}
		}
	}
	if g.maxUnstable > 0 && g.unstable >= g.maxUnstable {
		return g.backoff, true
	}
	return g.backoff, false
}

// markConnected records a successful reconnect at time now.
func (g *flapGovernor) markConnected(now time.Time) { g.lastConnected = now }

func (a *App) runSyncFollow(ctx context.Context, maxReconnect time.Duration, messagesStored, connectionEpoch *atomic.Int64, disconnected <-chan struct{}, staleReconnect <-chan staleReconnectRequest) (SyncResult, error) {
	gov := newFlapGovernor(nowUTC(), maxUnstableReconnects) // the initial connect already succeeded in Sync()

	// reconnectNow force-closes the (possibly stale-but-live-looking) socket, applies
	// flap backoff, and reconnects. It returns a non-nil error only when the daemon
	// should exit (so its supervisor can respawn it with a clean slate).
	reconnectNow := func(source string) error {
		if ctx.Err() != nil {
			return nil // shutting down; let the loop's ctx.Done() case return cleanly
		}
		backoff, giveUp := gov.beforeReconnect(nowUTC())
		if giveUp {
			return fmt.Errorf("connection flapping: %d consecutive reconnects failed to hold %s (%s); exiting for supervisor restart", gov.unstable, flapStableAfter, source)
		}
		if backoff > 0 {
			a.emitOrPrint("reconnect_backoff",
				map[string]any{"delay": backoff.String(), "unstable": gov.unstable, "source": source},
				"Connection flapping (%d); backing off %s before reconnect...\n", gov.unstable, backoff)
			if !flapSleep(ctx, backoff) {
				return nil // ctx cancelled during backoff; loop's ctx.Done() case returns next
			}
			if ctx.Err() != nil {
				return nil
			}
		}

		// Always force-close first so Client.Connect does a real reconnect instead of
		// no-oping on a socket that still looks live (matches the stale/StreamReplaced paths).
		a.wa.Close()
		connectionEpoch.Store(nowUTC().UnixNano())
		if err := a.reconnect(ctx, maxReconnect); err != nil {
			return err
		}
		gov.markConnected(nowUTC())
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			a.emitOrPrint("stopping", map[string]any{"messages_synced": messagesStored.Load()}, "\nStopping sync.\n")
			return SyncResult{MessagesStored: messagesStored.Load()}, nil
		case req := <-staleReconnect:
			if epoch := connectionEpoch.Load(); epoch > 0 && req.lastSuccess.Before(time.Unix(0, epoch)) {
				continue
			}
			a.emitOrPrint("stale", map[string]any{
				"threshold":     req.threshold.String(),
				"idle_duration": req.idle.String(),
				"error_count":   req.errorCount,
				"source":        req.source,
			}, "\nKeepalive has been failing for %s (threshold %s), reconnecting...\n", req.idle, req.threshold)
			if err := reconnectNow("keepalive"); err != nil {
				return SyncResult{MessagesStored: messagesStored.Load()}, err
			}
		case <-disconnected:
			a.emitOrPrint("reconnecting", nil, "Reconnecting...\n")
			if err := reconnectNow("disconnected"); err != nil {
				return SyncResult{MessagesStored: messagesStored.Load()}, err
			}
		}
	}
}

func (a *App) runSyncUntilIdle(ctx context.Context, idleExit, maxReconnect time.Duration, messagesStored, lastEvent *atomic.Int64, disconnected <-chan struct{}) (SyncResult, error) {
	poll := 250 * time.Millisecond
	if idleExit >= 2*time.Second {
		poll = 1 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.emitOrPrint("stopping", map[string]any{"messages_synced": messagesStored.Load()}, "\nStopping sync.\n")
			return SyncResult{MessagesStored: messagesStored.Load()}, nil
		case <-disconnected:
			a.emitOrPrint("reconnecting", nil, "Reconnecting...\n")
			if err := a.reconnect(ctx, maxReconnect); err != nil {
				return SyncResult{MessagesStored: messagesStored.Load()}, err
			}
		case <-ticker.C:
			last := time.Unix(0, lastEvent.Load())
			if time.Since(last) >= idleExit {
				a.emitOrPrint("idle_exit", map[string]any{
					"idle_duration":   idleExit.String(),
					"messages_synced": messagesStored.Load(),
				}, "\nIdle for %s, exiting.\n", idleExit)
				return SyncResult{MessagesStored: messagesStored.Load()}, nil
			}
		}
	}
}

// reconnect wraps ReconnectWithBackoff with an optional deadline. If maxDuration
// is positive, reconnection gives up after that long; otherwise it retries until
// ctx is cancelled.
func (a *App) reconnect(ctx context.Context, maxDuration time.Duration) error {
	rctx := ctx
	var cancel context.CancelFunc
	if maxDuration > 0 {
		rctx, cancel = context.WithTimeout(ctx, maxDuration)
		defer cancel()
	}
	err := a.wa.ReconnectWithBackoff(rctx, 2*time.Second, 30*time.Second)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("could not reconnect after %s: %w", maxDuration, err)
	}
	return err
}
