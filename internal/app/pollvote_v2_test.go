package app

import (
	"context"
	"errors"
	"sort"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestClassifyDecryptFailure(t *testing.T) {
	cases := map[string]string{
		"failed to decrypt poll vote: failed to decrypt secret message: cipher: message authentication failed": "mac_mismatch_all_realms",
		"failed to decrypt poll vote: original message secret key not found":                                   "message_secret_not_found",
		"given message isn't a poll update message":                                                            "not_poll_update",
		"some other failure": "other",
	}
	for input, want := range cases {
		if got := classifyDecryptFailure(errors.New(input)); got != want {
			t.Errorf("classifyDecryptFailure(%q) = %q, want %q", input, got, want)
		}
	}
}

func jidUser(user, server string) types.JID { return types.JID{User: user, Server: server} }

func groupWith(jidUserPart string, members ...types.JID) *types.GroupInfo {
	parts := make([]types.GroupParticipant, 0, len(members))
	for _, m := range members {
		parts = append(parts, types.GroupParticipant{JID: m})
	}
	return &types.GroupInfo{
		JID:          jidUser(jidUserPart, types.GroupServer),
		Participants: parts,
	}
}

func TestWarmGroupSessions_DedupesAcrossGroups(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f

	alice := jidUser("111", types.DefaultUserServer)
	bob := jidUser("222", types.DefaultUserServer)
	carol := jidUser("333", types.HiddenUserServer)

	g1 := groupWith("g1-1", alice, bob)
	g2 := groupWith("g2-2", bob, carol) // bob shared across both groups
	f.groups[g1.JID] = g1
	f.groups[g2.JID] = g2

	a.warmGroupSessions(context.Background(), "")

	got := append([]types.JID(nil), f.warmSessionsJIDs...)
	if len(got) != 3 {
		t.Fatalf("warmed %d members, want 3 (deduped): %v", len(got), got)
	}
	want := []string{alice.String(), bob.String(), carol.String()}
	gotStr := []string{got[0].String(), got[1].String(), got[2].String()}
	sort.Strings(want)
	sort.Strings(gotStr)
	for i := range want {
		if gotStr[i] != want[i] {
			t.Fatalf("warmed members = %v, want %v", gotStr, want)
		}
	}
}

func TestWarmGroupSessions_GroupFilter(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f

	alice := jidUser("111", types.DefaultUserServer)
	carol := jidUser("333", types.DefaultUserServer)
	g1 := groupWith("g1-1", alice)
	g2 := groupWith("g2-2", carol)
	f.groups[g1.JID] = g1
	f.groups[g2.JID] = g2

	a.warmGroupSessions(context.Background(), g2.JID.String())

	if len(f.warmSessionsJIDs) != 1 || f.warmSessionsJIDs[0].User != "333" {
		t.Fatalf("group filter warmed %v, want only carol (333)", f.warmSessionsJIDs)
	}
}

func TestWarmGroupSessions_NoMembersNoCall(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	a.warmGroupSessions(context.Background(), "") // no groups
	if len(f.warmSessionsJIDs) != 0 {
		t.Fatalf("expected no warm call with no groups, got %v", f.warmSessionsJIDs)
	}
}
