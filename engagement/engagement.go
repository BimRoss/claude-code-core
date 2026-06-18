// Package engagement records per-(workspace, user) Slack message counters
// for trial-to-paid conversion forecasting. Each bot (Ross, Joanne) calls
// Record on every human-authored channel message it sees; the Recorder
// batches and POSTs to makeacompany-ai, where /admin renders the rolled-up
// counters in an expandable row per Slack user.
//
// Two design constraints worth knowing:
//
//   - Non-blocking. Record never blocks the websocket dispatch loop. The
//     internal buffer is fixed-size; overflow drops the event with a log
//     warning rather than backpressuring inbound Slack traffic. The chance
//     of overflow is low — peak channel chatter is well under the batch
//     window — and dropping engagement counts is preferable to dropping
//     real bot replies.
//   - Self-contained. The Recorder owns its HTTP client and background
//     loop. Callers wire it once at startup and call Close at shutdown.
//
// Mention detection is done locally (regex on message text) so each event
// carries a boolean MentionsBot flag, lifting the work off the backend.
package engagement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack/slackevents"
)

// Event is one observed Slack message. WorkspaceID is the Slack team_id
// (not an internal UUID); SlackUserID is the message author. ChannelType
// is the raw Slack value ("channel", "im", "mpim", "group"); Text is
// retained only long enough to compute the MentionsBot flag and is not
// sent on the wire.
type Event struct {
	WorkspaceID string
	SlackUserID string
	ChannelID   string
	ChannelType string
	Text        string
	MessageTS   string
	OccurredAt  time.Time
}

// FromMessageEvent builds an Event from a slackevents.MessageEvent plus the
// team_id (which the slackevents struct does not expose; callers extract it
// from the raw envelope). Convenience for the common dispatch path.
func FromMessageEvent(workspaceID string, ev *slackevents.MessageEvent) Event {
	if ev == nil {
		return Event{}
	}
	return Event{
		WorkspaceID: workspaceID,
		SlackUserID: ev.User,
		ChannelID:   ev.Channel,
		ChannelType: ev.ChannelType,
		Text:        ev.Text,
		MessageTS:   ev.TimeStamp,
		OccurredAt:  time.Now().UTC(),
	}
}

// Config configures a Recorder. BaseURL, Token, and BotName are required.
// Other fields take sensible defaults documented below.
type Config struct {
	BaseURL    string
	Token      string
	BotName    string
	BotUserID  string
	HTTPClient *http.Client
	// BatchSize triggers an early flush when the buffer reaches this many
	// events. Default 50.
	BatchSize int
	// BatchWindow caps how long an event waits before being flushed.
	// Default 5s. Trades freshness against request rate.
	BatchWindow time.Duration
	// Logf receives warnings (buffer full, send failed). nil = silent.
	Logf func(format string, args ...any)
}

const (
	defaultBatchSize   = 50
	defaultBatchWindow = 5 * time.Second
	defaultHTTPTimeout = 5 * time.Second
	bufferSize         = 1024
	ingestPath         = "/v1/internal/ingest-user-engagement"
)

// Recorder accepts events from any goroutine and ships them to the sink in
// batches. Safe for concurrent use. Construct with New; tear down with Close.
type Recorder struct {
	cfg       Config
	in        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	mentionRE *regexp.Regexp
}

// New returns a started Recorder. Returns an error when BaseURL / Token /
// BotName are empty so a misconfigured pod fails fast on boot rather than
// silently dropping engagement data.
func New(cfg Config) (*Recorder, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("engagement: BaseURL required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("engagement: Token required")
	}
	if strings.TrimSpace(cfg.BotName) == "" {
		return nil, errors.New("engagement: BotName required")
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.BatchWindow <= 0 {
		cfg.BatchWindow = defaultBatchWindow
	}
	r := &Recorder{
		cfg:       cfg,
		in:        make(chan Event, bufferSize),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		mentionRE: regexp.MustCompile(`<@([A-Z0-9_]+)(?:\|[^>]*)?>`),
	}
	go r.loop()
	return r, nil
}

// Record submits an event for asynchronous batching. Non-blocking. Events
// with an empty SlackUserID or WorkspaceID are silently ignored — those
// can't be aggregated anyway. A nil receiver is a no-op so callers can use
// a single code path whether the recorder is wired or not.
func (r *Recorder) Record(ev Event) {
	if r == nil {
		return
	}
	if ev.SlackUserID == "" || ev.WorkspaceID == "" {
		return
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	select {
	case r.in <- ev:
	default:
		r.logf("engagement: buffer full, dropping event for user=%s workspace=%s", ev.SlackUserID, ev.WorkspaceID)
	}
}

// Close flushes pending events and stops the background loop. Returns when
// the in-flight batch has been sent or ctx is canceled, whichever comes
// first. Safe to call multiple times.
func (r *Recorder) Close(ctx context.Context) {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() { close(r.stop) })
	select {
	case <-r.done:
	case <-ctx.Done():
	}
}

func (r *Recorder) loop() {
	defer close(r.done)
	ticker := time.NewTicker(r.cfg.BatchWindow)
	defer ticker.Stop()
	buf := make([]wireEvent, 0, r.cfg.BatchSize)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.send(buf); err != nil {
			r.logf("engagement: send failed: %v", err)
		}
		buf = buf[:0]
	}
	for {
		select {
		case ev := <-r.in:
			buf = append(buf, r.toWire(ev))
			if len(buf) >= r.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-r.stop:
			for draining := true; draining; {
				select {
				case ev := <-r.in:
					buf = append(buf, r.toWire(ev))
				default:
					draining = false
				}
			}
			flush()
			return
		}
	}
}

type wireEvent struct {
	WorkspaceID string `json:"workspace_id"`
	SlackUserID string `json:"slack_user_id"`
	ChannelID   string `json:"channel_id"`
	ChannelType string `json:"channel_type,omitempty"`
	MessageTS   string `json:"message_ts,omitempty"`
	OccurredAt  string `json:"occurred_at"`
	MentionsBot bool   `json:"mentions_bot"`
}

type wireBatch struct {
	Bot    string      `json:"bot"`
	Events []wireEvent `json:"events"`
}

func (r *Recorder) toWire(ev Event) wireEvent {
	return wireEvent{
		WorkspaceID: ev.WorkspaceID,
		SlackUserID: ev.SlackUserID,
		ChannelID:   ev.ChannelID,
		ChannelType: ev.ChannelType,
		MessageTS:   ev.MessageTS,
		OccurredAt:  ev.OccurredAt.UTC().Format(time.RFC3339),
		MentionsBot: r.MentionsBot(ev.Text),
	}
}

// MentionsBot reports whether text contains an @-mention of the configured
// BotUserID. Returns false when BotUserID is unset. Exported so the same
// detector can be reused by callers that already have the text in hand.
func (r *Recorder) MentionsBot(text string) bool {
	if r == nil || r.cfg.BotUserID == "" || text == "" {
		return false
	}
	for _, m := range r.mentionRE.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 && m[1] == r.cfg.BotUserID {
			return true
		}
	}
	return false
}

func (r *Recorder) send(batch []wireEvent) error {
	body, err := json.Marshal(wireBatch{Bot: r.cfg.BotName, Events: batch})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.BaseURL+ingestPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (r *Recorder) logf(format string, args ...any) {
	if r.cfg.Logf != nil {
		r.cfg.Logf(format, args...)
	}
}
