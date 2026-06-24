package statusroute

import "testing"

func TestHeartbeatTarget(t *testing.T) {
	cases := []struct {
		name string
		in   Spawn
		want Target
	}{
		{
			// Human @-mention in a channel: handleMessage resolves threadTS to
			// the message's own ts when it isn't already in a thread, so there
			// is always a thread to nest the heartbeat under.
			name: "channel human reply threads under the message",
			in:   Spawn{ChannelSurface: true, ThreadTS: "1782319763.887179"},
			want: TargetThread,
		},
		{
			// Reply inside an existing thread — nest under that thread.
			name: "channel threaded reply",
			in:   Spawn{ChannelSurface: true, ThreadTS: "1782300000.000100"},
			want: TargetThread,
		},
		{
			// Managed loop (deploy-watcher, seed loops): the synthetic tick
			// carries the persistent anchor ts as its thread ts, so the
			// heartbeat threads under the anchor, same as the tick body.
			name: "managed loop tick threads under anchor",
			in:   Spawn{ChannelSurface: true, ThreadTS: "1782283200.697349"},
			want: TargetThread,
		},
		{
			// per_tick digest loop (chart, briefing, quote, triage): the
			// synthetic tick carries no thread ts because root is reserved for
			// the one digest line. The heartbeat must be suppressed, not
			// routed to root. This is the makeacompany-ai#676 leak.
			name: "per_tick digest loop suppresses the heartbeat",
			in:   Spawn{ChannelSurface: true, ThreadTS: ""},
			want: TargetSuppress,
		},
		{
			// DM with a thread ts present (a DM the agent replied in-thread on
			// some surfaces): still the DM root family, but a thread ts routes
			// it under the thread. Non-channel surface short-circuits to DM
			// root regardless, since a DM root is never a channel root.
			name: "dm is its own thread, root is fine",
			in:   Spawn{ChannelSurface: false, ThreadTS: ""},
			want: TargetDMRoot,
		},
		{
			name: "dm with thread ts still treated as dm root",
			in:   Spawn{ChannelSurface: false, ThreadTS: "1782300000.000200"},
			want: TargetDMRoot,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HeartbeatTarget(tc.in); got != tc.want {
				t.Errorf("HeartbeatTarget(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNeverChannelRoot is the invariant that motivates the package: across
// every channel-surface spawn, a heartbeat is either threaded or suppressed —
// it is never permitted to post at a channel root.
func TestNeverChannelRoot(t *testing.T) {
	for _, threadTS := range []string{"", "1782283200.697349", "1782319763.887179"} {
		got := HeartbeatTarget(Spawn{ChannelSurface: true, ThreadTS: threadTS})
		if got == TargetDMRoot {
			t.Errorf("channel surface (threadTS=%q) routed to a root post; heartbeats must never hit channel root", threadTS)
		}
	}
}

func TestShouldPostHeartbeat(t *testing.T) {
	cases := []struct {
		name string
		in   Spawn
		want bool
	}{
		{"per_tick digest loop is gated off", Spawn{ChannelSurface: true, ThreadTS: ""}, false},
		{"managed loop posts", Spawn{ChannelSurface: true, ThreadTS: "1782283200.697349"}, true},
		{"human thread posts", Spawn{ChannelSurface: true, ThreadTS: "1782319763.887179"}, true},
		{"dm posts", Spawn{ChannelSurface: false, ThreadTS: ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldPostHeartbeat(tc.in); got != tc.want {
				t.Errorf("ShouldPostHeartbeat(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
