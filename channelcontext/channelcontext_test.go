package channelcontext

import (
	"context"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func msg(ts, user, text string) slack.Message {
	m := slack.Message{}
	m.Timestamp = ts
	m.User = user
	m.Text = text
	return m
}

// TestIdentityHeader locks the three header shapes verbatim — the only thing
// that differed between the per-app copies before extraction.
func TestIdentityHeader(t *testing.T) {
	const trust = " These are the canonical IDs from env — trust them over any prose in CLAUDE.md.\n"
	cases := []struct {
		name string
		id   Identity
		want string
	}{
		{
			name: "self and peer (Ross)",
			id:   Identity{SelfName: "Ross", SelfID: "U_ROSS", PeerName: "Joanne", PeerID: "U_JOANNE"},
			want: "You are Ross, <@U_ROSS>. Joanne is <@U_JOANNE>." + trust,
		},
		{
			name: "self only (peer id empty)",
			id:   Identity{SelfName: "Ross", SelfID: "U_ROSS", PeerName: "Joanne"},
			want: "You are Ross, <@U_ROSS>." + trust,
		},
		{
			name: "no identity (personal-agent)",
			id:   Identity{},
			want: "",
		},
		{
			name: "peer only renders nothing",
			id:   Identity{PeerName: "Joanne", PeerID: "U_JOANNE"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.header(); got != tc.want {
				t.Errorf("header() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestFormat_emptyReturnsEmpty(t *testing.T) {
	if got := Format(nil, nil, "", Identity{SelfName: "Ross", SelfID: "U1"}); got != "" {
		t.Errorf("Format(empty) = %q, want \"\"", got)
	}
}

func TestFormat_rootAndThreadWithMarker(t *testing.T) {
	root := []slack.Message{
		msg("1700000000.000100", "U1", "hello\nworld"),
		msg("1700000300.000100", "U2", "second"),
	}
	// give the first root message thread metadata + make it the current thread
	root[0].ReplyCount = 2
	root[0].LatestReply = "1700000600.000100"
	root[0].ReplyUsers = []string{"U2", "U3"}

	thread := []slack.Message{
		msg("1700000000.000100", "U1", "hello world"),
		msg("1700000600.000100", "U3", "a reply"),
	}

	got := Format(root, thread, "1700000000.000100", Identity{SelfName: "Ross", SelfID: "U1", PeerName: "Joanne", PeerID: "U2"})

	for _, want := range []string{
		"<channel-context>\n",
		"You are Ross, <@U1>. Joanne is <@U2>.",
		"## Channel root (last 2 messages)\n",
		"hello ⏎ world",                          // newline compacted
		"(2 replies, last 22:23, with @U2, @U3)", // reply metadata
		"← you are here",                         // marker on the current-thread root
		"## Current thread (full)\n",
		"</channel-context>\n\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Format output missing %q.\n--- got ---\n%s", want, got)
		}
	}
	// The second root message must NOT carry the you-are-here marker.
	if strings.Count(got, "← you are here") != 1 {
		t.Errorf("expected exactly one you-are-here marker, got:\n%s", got)
	}
}

func TestCompactMessageText_truncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := compactMessageText(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) != 401 {
		t.Errorf("expected 400 chars + ellipsis, got len(runes)=%d", len([]rune(got)))
	}
}

type fakeAPI struct {
	hist    []slack.Message
	replies []slack.Message
	histErr error
}

func (f fakeAPI) GetConversationHistoryContext(_ context.Context, _ *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	if f.histErr != nil {
		return nil, f.histErr
	}
	return &slack.GetConversationHistoryResponse{Messages: f.hist}, nil
}

func (f fakeAPI) GetConversationRepliesContext(_ context.Context, _ *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	return f.replies, false, "", nil
}

func TestFetch_returnsBlock(t *testing.T) {
	api := fakeAPI{hist: []slack.Message{msg("1700000000.000100", "U1", "hi")}}
	got := Fetch(context.Background(), api, "C1", "", Identity{SelfName: "Joanne", SelfID: "U_J"})
	if !strings.Contains(got, "<channel-context>") || !strings.Contains(got, "You are Joanne, <@U_J>.") {
		t.Errorf("Fetch did not return a populated block:\n%s", got)
	}
}

func TestFetch_failOpenOnHistoryError(t *testing.T) {
	api := fakeAPI{histErr: context.DeadlineExceeded}
	if got := Fetch(context.Background(), api, "C1", "", Identity{}); got != "" {
		t.Errorf("Fetch on history error = %q, want \"\" (fail-open)", got)
	}
}
