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
			// Managed loop tick (deploy-watcher, seed loops): synthetic, no
			// operator waiting. Suppress even though it carries an anchor ts.
			name: "managed loop tick suppresses the heartbeat",
			in:   Spawn{ChannelSurface: true, ThreadTS: "1782283200.697349", IsLoopTick: true},
			want: TargetSuppress,
		},
		{
			// Threaded loop tick (the #461 default: chart, briefing, triage):
			// carries an anchor thread ts, but it is still a synthetic tick with
			// no operator. Suppress — without IsLoopTick the non-empty ThreadTS
			// would wrongly route it to TargetThread, the #461 regression.
			name: "threaded loop tick suppresses despite anchor ts",
			in:   Spawn{ChannelSurface: true, ThreadTS: "1782320738.845949", IsLoopTick: true},
			want: TargetSuppress,
		},
		{
			// per_tick digest loop (opt-in legacy): no thread ts and a loop
			// tick. Either gate suppresses it; this is the makeacompany-ai#676
			// leak shape.
			name: "per_tick digest loop suppresses the heartbeat",
			in:   Spawn{ChannelSurface: true, ThreadTS: "", IsLoopTick: true},
			want: TargetSuppress,
		},
		{
			// Defense in depth: a channel spawn with no thread ts that somehow
			// is not flagged a loop tick still suppresses rather than risk root.
			name: "channel surface, no thread, not flagged still suppresses",
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
		{"per_tick loop tick is gated off", Spawn{ChannelSurface: true, ThreadTS: "", IsLoopTick: true}, false},
		{"threaded loop tick gated off despite anchor ts", Spawn{ChannelSurface: true, ThreadTS: "1782320738.845949", IsLoopTick: true}, false},
		{"managed loop tick is gated off", Spawn{ChannelSurface: true, ThreadTS: "1782283200.697349", IsLoopTick: true}, false},
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
