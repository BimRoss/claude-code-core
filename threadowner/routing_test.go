package threadowner

import "testing"

func TestDecide_NewThreadGenericGoesToRoss(t *testing.T) {
	// From Ross's perspective.
	got := Decide(OwnerRoss, "", false, /*mentionsMe*/ false, /*mentionsOther*/ false, /*isNewThread*/ true)
	if !got.Respond || got.NewOwner != OwnerRoss {
		t.Errorf("ross: new-thread generic should respond and own; got %+v", got)
	}
	// From Joanne's perspective.
	got = Decide(OwnerJoanne, "", false, false, false, true)
	if got.Respond {
		t.Errorf("joanne: new-thread generic should NOT respond; got %+v", got)
	}
	if got.NewOwner != OwnerRoss {
		t.Errorf("joanne: new-thread generic owner should be ross; got %q", got.NewOwner)
	}
}

func TestDecide_NewThreadAtJoanneOnly(t *testing.T) {
	// Ross sees @Joanne only: stay silent, predict owner=joanne.
	got := Decide(OwnerRoss, "", false, false, true, true)
	if got.Respond {
		t.Errorf("ross: @Joanne-only should not respond; got %+v", got)
	}
	if got.NewOwner != OwnerJoanne {
		t.Errorf("ross: predicted owner should be joanne; got %q", got.NewOwner)
	}
	// Joanne sees @Joanne only: respond + own.
	got = Decide(OwnerJoanne, "", false, true, false, true)
	if !got.Respond || got.NewOwner != OwnerJoanne {
		t.Errorf("joanne: @Joanne-only should respond and own; got %+v", got)
	}
}

func TestDecide_NewThreadDualMentionBothRespondRossOwns(t *testing.T) {
	got := Decide(OwnerRoss, "", false, true, true, true)
	if !got.Respond || got.NewOwner != OwnerRoss {
		t.Errorf("ross: dual-mention should respond and own; got %+v", got)
	}
	got = Decide(OwnerJoanne, "", false, true, true, true)
	if !got.Respond || got.NewOwner != OwnerRoss {
		t.Errorf("joanne: dual-mention should respond and ross owns; got %+v", got)
	}
}

func TestDecide_InThreadGenericRoutesToOwner(t *testing.T) {
	// Joanne owns; generic in-thread.
	got := Decide(OwnerJoanne, OwnerJoanne, true, false, false, false)
	if !got.Respond {
		t.Errorf("joanne owner: should respond; got %+v", got)
	}
	if got.NewOwner != "" {
		t.Errorf("joanne owner: no change expected; got %q", got.NewOwner)
	}
	// Ross side: Joanne owns, Ross stays silent.
	got = Decide(OwnerRoss, OwnerJoanne, true, false, false, false)
	if got.Respond {
		t.Errorf("ross when joanne owns: should not respond; got %+v", got)
	}
	if got.NewOwner != "" {
		t.Errorf("ross when joanne owns: no change expected; got %q", got.NewOwner)
	}
}

func TestDecide_InThreadGenericNoOwnerFallsBackToRoss(t *testing.T) {
	// Pre-existing thread, no record. Ross-default.
	got := Decide(OwnerRoss, "", false, false, false, false)
	if !got.Respond {
		t.Errorf("ross fallback: should respond; got %+v", got)
	}
	if got.NewOwner != OwnerRoss {
		t.Errorf("ross fallback: should write owner=ross; got %q", got.NewOwner)
	}
	got = Decide(OwnerJoanne, "", false, false, false, false)
	if got.Respond {
		t.Errorf("joanne fallback: should not respond; got %+v", got)
	}
}

func TestDecide_InThreadAtOtherFlipsOwnership(t *testing.T) {
	// Joanne currently owns; user @Ross in the thread.
	// Ross side: respond + claim.
	got := Decide(OwnerRoss, OwnerJoanne, true, true, false, false)
	if !got.Respond || got.NewOwner != OwnerRoss {
		t.Errorf("ross flip: should respond and own; got %+v", got)
	}
	// Joanne side: skip + predict owner=ross.
	got = Decide(OwnerJoanne, OwnerJoanne, true, false, true, false)
	if got.Respond {
		t.Errorf("joanne flip: should NOT respond; got %+v", got)
	}
	if got.NewOwner != OwnerRoss {
		t.Errorf("joanne flip: predicted owner should be ross; got %q", got.NewOwner)
	}
}

func TestDecide_InThreadAtOwnerIsIdempotent(t *testing.T) {
	// Joanne owns; user @Joannes again. She responds; owner stays joanne.
	got := Decide(OwnerJoanne, OwnerJoanne, true, true, false, false)
	if !got.Respond || got.NewOwner != OwnerJoanne {
		t.Errorf("joanne re-mention: should respond, owner stays joanne; got %+v", got)
	}
}

// DecideWithLock locked-channel matrix. Welcome channel = Joanne-locked.
func TestDecideWithLock_EmptyLockMatchesDecide(t *testing.T) {
	a := DecideWithLock(OwnerRoss, "", "", false, false, false, true)
	b := Decide(OwnerRoss, "", false, false, false, true)
	if a != b {
		t.Errorf("empty lock should delegate to Decide; got %+v vs %+v", a, b)
	}
}

func TestDecideWithLock_NewThreadNoMentionRoutesToLockOwner(t *testing.T) {
	// Welcome channel, new root message, plain text. Joanne responds; Ross drops.
	got := DecideWithLock(OwnerJoanne, OwnerJoanne, "", false, false, false, true)
	if !got.Respond || got.NewOwner != OwnerJoanne {
		t.Errorf("joanne locked-default: should respond and own; got %+v", got)
	}
	got = DecideWithLock(OwnerRoss, OwnerJoanne, "", false, false, false, true)
	if got.Respond {
		t.Errorf("ross locked-other: should NOT respond; got %+v", got)
	}
	if got.NewOwner != OwnerJoanne {
		t.Errorf("ross locked-other: predicted owner stays joanne; got %q", got.NewOwner)
	}
}

func TestDecideWithLock_AtRossInLockedChannelRossResponds(t *testing.T) {
	// Welcome channel, explicit @Ross. Ross responds. Joanne drops.
	// Crucially, NewOwner stays Joanne — no flip.
	got := DecideWithLock(OwnerRoss, OwnerJoanne, "", false, true, false, true)
	if !got.Respond {
		t.Errorf("ross @-mentioned in locked channel: should respond; got %+v", got)
	}
	if got.NewOwner != OwnerJoanne {
		t.Errorf("ross @-mentioned in locked channel: owner stays joanne; got %q", got.NewOwner)
	}
	got = DecideWithLock(OwnerJoanne, OwnerJoanne, "", false, false, true, true)
	if got.Respond {
		t.Errorf("joanne when ross @-mentioned: should NOT respond; got %+v", got)
	}
}

func TestDecideWithLock_AtJoanneInLockedChannelJoanneResponds(t *testing.T) {
	got := DecideWithLock(OwnerJoanne, OwnerJoanne, "", false, true, false, true)
	if !got.Respond || got.NewOwner != OwnerJoanne {
		t.Errorf("joanne @-mentioned locked: should respond and own; got %+v", got)
	}
	got = DecideWithLock(OwnerRoss, OwnerJoanne, "", false, false, true, true)
	if got.Respond {
		t.Errorf("ross when joanne @-mentioned: should NOT respond; got %+v", got)
	}
}

func TestDecideWithLock_DualMentionBothRespondOwnerStays(t *testing.T) {
	// Per Grant's call: dual-mention is the human asking for both. Both
	// respond. Ownership stays with lockedTo.
	got := DecideWithLock(OwnerRoss, OwnerJoanne, "", false, true, true, true)
	if !got.Respond || got.NewOwner != OwnerJoanne {
		t.Errorf("ross dual-mention locked: should respond, owner joanne; got %+v", got)
	}
	got = DecideWithLock(OwnerJoanne, OwnerJoanne, "", false, true, true, true)
	if !got.Respond || got.NewOwner != OwnerJoanne {
		t.Errorf("joanne dual-mention locked: should respond, owner joanne; got %+v", got)
	}
}

func TestDecideWithLock_InThreadGenericRoutesToLockOwner(t *testing.T) {
	// Stored owner from an unlocked-era message says Ross. Locked policy
	// overrides: Joanne still responds in welcome.
	got := DecideWithLock(OwnerJoanne, OwnerJoanne, OwnerRoss, true, false, false, false)
	if !got.Respond {
		t.Errorf("locked policy should override stored owner; got %+v", got)
	}
	if got.NewOwner != OwnerJoanne {
		t.Errorf("locked policy should rewrite owner=joanne; got %q", got.NewOwner)
	}
	got = DecideWithLock(OwnerRoss, OwnerJoanne, OwnerRoss, true, false, false, false)
	if got.Respond {
		t.Errorf("ross in locked channel, no mention: should NOT respond even if stored owner=ross; got %+v", got)
	}
}

func TestParseChannelLocks(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]Owner
	}{
		{"", map[string]Owner{}},
		{"C0APJT7PYUU:joanne", map[string]Owner{"C0APJT7PYUU": OwnerJoanne}},
		{" C0APJT7PYUU : joanne , C0XXX:ross ", map[string]Owner{"C0APJT7PYUU": OwnerJoanne, "C0XXX": OwnerRoss}},
		{"C0APJT7PYUU:bogus", map[string]Owner{}},
		{"malformed,C0OK:ross", map[string]Owner{"C0OK": OwnerRoss}},
		{"C0EMPTY:", map[string]Owner{}},
		{":joanne", map[string]Owner{}},
	}
	for _, c := range cases {
		got := ParseChannelLocks(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ParseChannelLocks(%q) size mismatch: got %v, want %v", c.in, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("ParseChannelLocks(%q)[%q]: got %q, want %q", c.in, k, got[k], v)
			}
		}
	}
}
