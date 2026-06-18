package engagement

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNew_RequiresConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing url", Config{Token: "t", BotName: "ross"}, "BaseURL"},
		{"missing token", Config{BaseURL: "http://x", BotName: "ross"}, "Token"},
		{"missing bot", Config{BaseURL: "http://x", Token: "t"}, "BotName"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error mentioning %q", err, c.want)
			}
		})
	}
}

func TestRecorder_BatchesByCount(t *testing.T) {
	var (
		mu        sync.Mutex
		gotBatch  wireBatch
		bodyCount int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("auth header = %q", got)
		}
		var b wireBatch
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &b)
		mu.Lock()
		gotBatch = b
		bodyCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, err := New(Config{
		BaseURL:     srv.URL,
		Token:       "secret",
		BotName:     "ross",
		BotUserID:   "UBOT",
		BatchSize:   3,
		BatchWindow: time.Hour, // do not rely on the timer in this test
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		r.Record(Event{WorkspaceID: "T1", SlackUserID: "U1", ChannelID: "C1", Text: "hi <@UBOT>"})
	}

	// allow the loop a beat to flush
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return bodyCount == 1
	})

	mu.Lock()
	defer mu.Unlock()
	if gotBatch.Bot != "ross" {
		t.Errorf("bot = %q, want ross", gotBatch.Bot)
	}
	if len(gotBatch.Events) != 3 {
		t.Fatalf("len events = %d, want 3", len(gotBatch.Events))
	}
	for _, e := range gotBatch.Events {
		if !e.MentionsBot {
			t.Errorf("expected MentionsBot=true for text with <@UBOT>")
		}
	}
}

func TestRecorder_BatchesByWindow(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, err := New(Config{
		BaseURL:     srv.URL,
		Token:       "t",
		BotName:     "ross",
		BatchSize:   100,
		BatchWindow: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Record(Event{WorkspaceID: "T1", SlackUserID: "U1"})
	waitFor(t, time.Second, func() bool { return calls.Load() >= 1 })
}

func TestRecorder_DropsIncompleteEvents(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, err := New(Config{
		BaseURL:     srv.URL,
		Token:       "t",
		BotName:     "ross",
		BatchSize:   1, // force immediate flush
		BatchWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Record(Event{SlackUserID: "U1"})              // missing workspace
	r.Record(Event{WorkspaceID: "T1"})              // missing user
	r.Record(Event{})                               // empty
	r.Record(Event{WorkspaceID: "T1", SlackUserID: "U1"}) // valid

	waitFor(t, time.Second, func() bool { return calls.Load() == 1 })
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want exactly 1 (only valid event)", got)
	}
}

func TestRecorder_NilSafe(t *testing.T) {
	var r *Recorder
	r.Record(Event{WorkspaceID: "T1", SlackUserID: "U1"})
	r.Close(context.Background())
	if r.MentionsBot("hi") {
		t.Error("nil recorder should not match mentions")
	}
}

func TestRecorder_FlushesOnClose(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, err := New(Config{
		BaseURL:     srv.URL,
		Token:       "t",
		BotName:     "ross",
		BatchSize:   1000,
		BatchWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Record(Event{WorkspaceID: "T1", SlackUserID: "U1"})
	r.Close(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (close should flush)", got)
	}
}

func TestMentionsBot(t *testing.T) {
	r, err := New(Config{BaseURL: "http://x", Token: "t", BotName: "ross", BotUserID: "UROSS"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })

	cases := []struct {
		text string
		want bool
	}{
		{"hello world", false},
		{"hey <@UROSS> please look", true},
		{"hey <@UROSS|ross> please look", true},
		{"hey <@OTHER>", false},
		{"<@UROSS> <@OTHER>", true},
		{"", false},
	}
	for _, c := range cases {
		if got := r.MentionsBot(c.text); got != c.want {
			t.Errorf("MentionsBot(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestRecorder_500_DoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var logged atomic.Int32
	r, err := New(Config{
		BaseURL:     srv.URL,
		Token:       "t",
		BotName:     "ross",
		BatchSize:   1,
		BatchWindow: time.Hour,
		Logf:        func(string, ...any) { logged.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Record(Event{WorkspaceID: "T1", SlackUserID: "U1"})
	waitFor(t, time.Second, func() bool { return logged.Load() >= 1 })
	r.Close(context.Background())
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}
