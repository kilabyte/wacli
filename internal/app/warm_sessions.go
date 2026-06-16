package app

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow/types"
)

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
		if jid, perr := types.ParseJID(groupFilter); perr == nil {
			want = &jid
		} else {
			a.emitWarning("warm_sessions_bad_group",
				fmt.Sprintf("warning: warm-sessions ignoring invalid --warm-group %q: %v", groupFilter, perr),
				map[string]any{"error": perr.Error()})
		}
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
	a.emitOrPrint("warm_sessions",
		map[string]any{"members": len(members), "groups": groupCount, "devices": devices},
		"Warmed %d group member(s) across %d group(s): %d devices refreshed.\n", len(members), groupCount, devices)
}
