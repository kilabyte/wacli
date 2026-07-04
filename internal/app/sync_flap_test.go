package app

import (
	"testing"
	"time"
)

// TestFlapGovernorStableDropNoBackoff: a single drop after the connection had been up
// longer than flapStableAfter is a fresh, isolated drop — no backoff, no give-up.
func TestFlapGovernorStableDropNoBackoff(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	g := newFlapGovernor(base, maxUnstableReconnects)

	backoff, giveUp := g.beforeReconnect(base.Add(flapStableAfter + time.Second))
	if backoff != 0 {
		t.Fatalf("stable drop should not back off, got %s", backoff)
	}
	if giveUp {
		t.Fatal("stable drop should not give up")
	}
	if g.unstable != 0 {
		t.Fatalf("stable drop should not count as unstable, got %d", g.unstable)
	}
}

// TestFlapGovernorBackoffProgression: rapid reconnects (each connection lives only a
// short time, < flapStableAfter) grow the backoff 1s,2s,4s,8s,16s,30s,30s and never give
// up when maxUnstable is 0 (backoff alone bounds the rate).
func TestFlapGovernorBackoffProgression(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	g := newFlapGovernor(base, 0) // 0 = never give up

	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second, // capped
	}
	now := base
	for i, w := range want {
		now = now.Add(2 * time.Second) // brief uptime since last connect (a flap)
		backoff, giveUp := g.beforeReconnect(now)
		if giveUp {
			t.Fatalf("iteration %d: unexpected give-up with maxUnstable=0", i)
		}
		if backoff != w {
			t.Fatalf("iteration %d: backoff = %s, want %s", i, backoff, w)
		}
		now = now.Add(backoff) // simulate the backoff sleep
		g.markConnected(now)   // the reconnect succeeded (then will drop again)
	}
}

// TestFlapGovernorGivesUpAfterConsecutiveUnstableReconnects: a sustained flap eventually
// returns giveUp=true so the daemon exits and its supervisor respawns it with a clean
// slate. Give-up is keyed on the unstable count, not wall-clock.
func TestFlapGovernorGivesUpAfterConsecutiveUnstableReconnects(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	const limit = 5
	g := newFlapGovernor(base, limit)

	now := base
	gaveUpAt := 0
	for i := 1; i <= limit*2; i++ {
		now = now.Add(2 * time.Second) // brief uptime each cycle => flapping
		_, giveUp := g.beforeReconnect(now)
		if giveUp {
			gaveUpAt = i
			break
		}
		now = now.Add(g.backoff)
		g.markConnected(now)
	}
	if gaveUpAt != limit {
		t.Fatalf("gave up after %d unstable reconnects, want exactly %d", gaveUpAt, limit)
	}
}

// TestFlapGovernorResetsAfterStablePeriod: after some flapping, a connection that then
// holds longer than flapStableAfter resets the governor — the unstable counter and backoff
// return to zero, so an earlier flap streak cannot push a later, unrelated blip into give-up.
func TestFlapGovernorResetsAfterStablePeriod(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	g := newFlapGovernor(base, maxUnstableReconnects)

	// A short flap streak.
	now := base
	for i := 0; i < 4; i++ {
		now = now.Add(2 * time.Second)
		g.beforeReconnect(now)
		now = now.Add(g.backoff)
		g.markConnected(now)
	}
	if g.unstable != 4 {
		t.Fatalf("expected 4 unstable reconnects, got %d", g.unstable)
	}

	// Now the connection holds well past flapStableAfter, then drops once.
	backoff, giveUp := g.beforeReconnect(now.Add(flapStableAfter + time.Minute))
	if backoff != 0 {
		t.Fatalf("drop after a stable period should reset backoff to 0, got %s", backoff)
	}
	if giveUp {
		t.Fatal("a single drop after recovery must not give up")
	}
	if g.unstable != 0 {
		t.Fatalf("recovery should reset the unstable counter, got %d", g.unstable)
	}
}

// TestFlapGovernorNormalBlipsNeverGiveUp: intermittent single drops, each followed by a
// connection that holds past flapStableAfter (the ordinary all-day pattern), must never
// accumulate toward give-up no matter how many occur.
func TestFlapGovernorNormalBlipsNeverGiveUp(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	g := newFlapGovernor(base, maxUnstableReconnects)

	now := base
	for i := 0; i < maxUnstableReconnects*3; i++ {
		now = now.Add(10 * time.Minute) // a long, healthy connection between blips
		_, giveUp := g.beforeReconnect(now)
		if giveUp {
			t.Fatalf("blip %d wrongly triggered give-up", i)
		}
		g.markConnected(now)
	}
	if g.unstable != 0 {
		t.Fatalf("healthy blips left unstable counter at %d, want 0", g.unstable)
	}
}
