package app

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// warmSessionsTimeout bounds the background warm usync so it can't hold whatsmeow's device-cache
// lock indefinitely (a single usync IQ can otherwise block up to the ~75s IQ timeout).
const warmSessionsTimeout = 30 * time.Second

// minWarmInterval floors the re-warm cadence so a misconfiguration can't hammer usync.
const minWarmInterval = 1 * time.Minute

// runSessionWarmer warms once immediately, then (if interval > 0) re-warms on a ticker until ctx is
// done. Used by a long-lived `sync --follow --warm-sessions --warm-interval` to keep members' device
// lists fresh through the whole voting window. Each warm is bounded by warmSessionsTimeout.
func (a *App) runSessionWarmer(ctx context.Context, group string, interval time.Duration) {
	warmOnce := func() {
		warmCtx, cancel := context.WithTimeout(ctx, warmSessionsTimeout)
		defer cancel()
		a.warmGroupSessions(warmCtx, group)
	}
	warmOnce()
	if interval <= 0 {
		return
	}
	if interval < minWarmInterval {
		interval = minWarmInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			warmOnce()
		}
	}
}

// warmGroupSessions refreshes the device lists / PN<->LID mappings for members of joined groups via
// a usync query right after connecting. Opt-in (--warm-sessions); off by default.
//
// It sends nothing user-visible; it only freshens our local routing/identity view so members who
// recently migrated realm (PN->LID) are recognised sooner, and populates whatsmeow_lid_map for the
// decrypt fallback. Its effect on whether *other* clients route early votes to us is best-effort and
// unverified; staying connected through the voting window (sync --follow --for) is the stronger
// lever. All failures are non-fatal warnings.
func (a *App) warmGroupSessions(ctx context.Context, groupFilter string) {
	groups, err := a.wa.GetJoinedGroups(ctx)
	if err != nil {
		a.emitWarning("warm_sessions_groups_failed",
			fmt.Sprintf("warning: warm-sessions could not list groups: %v", err),
			map[string]any{"error": err.Error()})
		return
	}

	var want *types.JID
	if groupFilter != "" {
		jid, perr := types.ParseJID(groupFilter)
		if perr != nil {
			// Fail closed: a bad --warm-group must NOT silently widen warming to every joined
			// group (the opposite of the intended scope). Skip warming entirely.
			a.emitWarning("warm_sessions_bad_group",
				fmt.Sprintf("warning: warm-sessions skipped: invalid --warm-group %q: %v", groupFilter, perr),
				map[string]any{"error": perr.Error()})
			return
		}
		want = &jid
	}

	seen := make(map[types.JID]struct{})
	var members []types.JID
	groupCount := 0
	for _, g := range groups {
		if g == nil {
			continue
		}
		if want != nil && g.JID.ToNonAD() != want.ToNonAD() {
			continue
		}
		groupCount++
		for _, p := range g.Participants {
			j := p.JID.ToNonAD()
			if j.IsEmpty() {
				continue
			}
			if _, ok := seen[j]; ok {
				continue
			}
			seen[j] = struct{}{}
			members = append(members, j)
		}
	}
	if len(members) == 0 {
		return
	}

	devices, err := a.wa.WarmSessions(ctx, members)
	if err != nil {
		a.emitWarning("warm_sessions_failed",
			fmt.Sprintf("warning: warm-sessions usync failed after %d members: %v", len(members), err),
			map[string]any{"error": err.Error(), "members": len(members), "groups": groupCount})
		return
	}
	// A successful warm proves a live, usable connection (it ran real usync IQs), so
	// refresh the heartbeat. In follow mode this fires every --warm-interval, giving a
	// supervisor a reliable connection-health signal even in a quiet group with no
	// inbound messages. (A wedged/dead socket fails GetJoinedGroups above and returns
	// early, so the heartbeat correctly goes stale.)
	a.writeHeartbeat()
	a.emitOrPrint("warm_sessions",
		map[string]any{"members": len(members), "groups": groupCount, "devices": devices},
		"Warmed %d group member(s) across %d group(s): %d devices refreshed.\n", len(members), groupCount, devices)
}
