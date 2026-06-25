package oauthpool

import (
	"testing"
	"time"
)

func resetCounter() {
	counter.Lock()
	counter.n = 0
	counter.Unlock()
}

func TestFromEnviron_DiscoversNSlots(t *testing.T) {
	p := fromEnviron([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=a",
		"CLAUDE_CODE_OAUTH_TOKEN_2=b",
		"CLAUDE_CODE_OAUTH_TOKEN_3=c",
		"UNRELATED=x",
	})
	if p.Size() != 3 {
		t.Fatalf("Size=%d want 3", p.Size())
	}
	want := []string{"1", "2", "3"}
	got := p.Labels()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels=%v want %v", got, want)
		}
	}
}

func TestFromEnviron_SkipsEmptyAndHandlesGaps(t *testing.T) {
	// slot 2 empty, slot 4 present (gap at 3) — only populated slots, sorted.
	p := fromEnviron([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=a",
		"CLAUDE_CODE_OAUTH_TOKEN_2=",
		"CLAUDE_CODE_OAUTH_TOKEN_4=d",
	})
	if p.Size() != 2 {
		t.Fatalf("Size=%d want 2", p.Size())
	}
	got := p.Labels()
	if got[0] != "1" || got[1] != "4" {
		t.Fatalf("labels=%v want [1 4]", got)
	}
}

func TestNext_RoundRobinsOverPopulated(t *testing.T) {
	resetCounter()
	p := fromEnviron([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=a",
		"CLAUDE_CODE_OAUTH_TOKEN_2=b",
		"CLAUDE_CODE_OAUTH_TOKEN_3=c",
	})
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		s, ok := p.Next()
		if !ok {
			t.Fatal("Next ok=false on populated pool")
		}
		seen[s.Label]++
	}
	for _, l := range []string{"1", "2", "3"} {
		if seen[l] != 3 {
			t.Fatalf("slot %s drawn %d times over 9 spawns, want 3 (even round-robin): %v", l, seen[l], seen)
		}
	}
}

func TestNext_SingleSlot(t *testing.T) {
	resetCounter()
	p := fromEnviron([]string{"CLAUDE_CODE_OAUTH_TOKEN=a"})
	s, ok := p.Next()
	if !ok || s.Label != "1" || s.Token != "a" {
		t.Fatalf("got %+v ok=%v want slot 1 token a", s, ok)
	}
}

func TestNext_EmptyPool(t *testing.T) {
	p := fromEnviron(nil)
	if _, ok := p.Next(); ok {
		t.Fatal("Next ok=true on empty pool")
	}
	if p.HasAlternate("1") {
		t.Fatal("HasAlternate true on empty pool")
	}
}

func TestOthers_ExcludesTriedAndCooled(t *testing.T) {
	resetCounter()
	p := fromEnviron([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=a",
		"CLAUDE_CODE_OAUTH_TOKEN_2=b",
		"CLAUDE_CODE_OAUTH_TOKEN_3=c",
	})
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return base }
	defer func() { now = time.Now }()

	p.MarkLimited("3", base.Add(time.Hour)) // slot 3 cooling
	others := p.Others(map[string]bool{"1": true})
	if len(others) != 1 || others[0].Label != "2" {
		t.Fatalf("others=%v want only slot 2 (1 tried, 3 cooled)", labels(others))
	}
}

func TestNext_SkipsCooledDown(t *testing.T) {
	resetCounter()
	p := fromEnviron([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=a",
		"CLAUDE_CODE_OAUTH_TOKEN_2=b",
	})
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return base }
	defer func() { now = time.Now }()

	p.MarkLimited("1", base.Add(time.Hour))
	for i := 0; i < 4; i++ {
		s, _ := p.Next()
		if s.Label != "2" {
			t.Fatalf("draw %d = slot %s, want slot 2 (slot 1 cooled)", i, s.Label)
		}
	}
}

func TestNext_AllCooledReturnsSoonest(t *testing.T) {
	resetCounter()
	p := fromEnviron([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=a",
		"CLAUDE_CODE_OAUTH_TOKEN_2=b",
	})
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return base }
	defer func() { now = time.Now }()

	p.MarkLimited("1", base.Add(2*time.Hour))
	p.MarkLimited("2", base.Add(30*time.Minute)) // recovers soonest
	s, ok := p.Next()
	if !ok || s.Label != "2" {
		t.Fatalf("got %s ok=%v want slot 2 (soonest recovery)", s.Label, ok)
	}
}

func TestMarkLimited_PastTimeClears(t *testing.T) {
	resetCounter()
	p := fromEnviron([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=a",
		"CLAUDE_CODE_OAUTH_TOKEN_2=b",
	})
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return base }
	defer func() { now = time.Now }()

	p.MarkLimited("1", base.Add(time.Hour))
	p.MarkLimited("1", base.Add(-time.Minute)) // clears
	if !p.HasAlternate("2") {
		t.Fatal("slot 1 should be available again after past-time clear")
	}
}

func TestHasAlternate(t *testing.T) {
	resetCounter()
	single := fromEnviron([]string{"CLAUDE_CODE_OAUTH_TOKEN=a"})
	if single.HasAlternate("1") {
		t.Fatal("single-slot pool should have no alternate")
	}
	dual := fromEnviron([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=a",
		"CLAUDE_CODE_OAUTH_TOKEN_2=b",
	})
	if !dual.HasAlternate("1") {
		t.Fatal("two-slot pool should have an alternate to slot 1")
	}
}

func TestScrub(t *testing.T) {
	got := scrub([]string{
		"PATH=/bin",
		"ANTHROPIC_API_KEY=secret",
		"ANTHROPIC_AUTH_TOKEN=secret2",
		"CLAUDE_CODE_OAUTH_TOKEN=keep",
	})
	for _, kv := range got {
		if kv == "ANTHROPIC_API_KEY=secret" || kv == "ANTHROPIC_AUTH_TOKEN=secret2" {
			t.Fatalf("scrub left a provider key: %q", kv)
		}
	}
	if len(got) != 2 {
		t.Fatalf("scrub kept %d vars want 2 (PATH + OAUTH token)", len(got))
	}
}

func labels(ss []Slot) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Label
	}
	return out
}
