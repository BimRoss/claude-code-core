// Package streaming is the shared `claude --output-format stream-json` consumer
// for all three agents (Ross, Joanne, personal-agent).
//
// It owns: the stream-json decode loop, the milestone catalog (one shared set of
// "the agent is changing the world" bash/write patterns → short Slack lines),
// the milestone debounce, the Google-Workspace tool-call audit log, and secret
// redaction for log previews. Per the 2026-06-25 "unify more" decision, the
// catalog, GWS audit, and redaction are SHARED across all agents rather than
// living in one and drifting.
//
// Genuinely per-agent behavior is injected via Config hooks:
//   - OnToolUse: fires for every tool_use block (Joanne uses it to observe a
//     deploy-shape command and later consume the free-tier deploy gate).
//   - SuppressCategories: drop specific milestone categories (Ross suppresses
//     "release-tag" because its deploy-watcher anchor already covers the full
//     tag-push lifecycle in a single managed message).
package streaming

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultDebounce is the hard floor between milestone posts (and between the
// last reply of any kind and a new milestone). Sits well under the handler's
// fallback heartbeat so a fast happy path gets a few meaningful lines without
// exceeding the noise ceiling.
const DefaultDebounce = 90 * time.Second

// ContentBlock is one assistant content block; only the fields we use.
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type streamEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Message json.RawMessage `json:"message"`
	Result  string          `json:"result"`
	IsError bool            `json:"is_error"`
}

type assistantMessage struct {
	Content []ContentBlock `json:"content"`
}

// Config parameterizes a stream consumption. Reply and LastPostedAt are
// required; the rest are optional per-agent hooks.
type Config struct {
	// Reply posts a line to Slack (the handler's gated/debounced reply closure;
	// it updates LastPostedAt).
	Reply func(string)
	// LastPostedAt is the shared monotonic timestamp the handler's fallback
	// ticker also reads; used to enforce the milestone debounce floor.
	LastPostedAt *atomic.Int64
	// Debounce overrides DefaultDebounce when > 0.
	Debounce time.Duration
	// OnToolUse, if set, fires for every tool_use block (in stream order),
	// before milestone classification. For agent-specific observation that
	// isn't a shared milestone (e.g. Joanne's deploy-gate observe).
	OnToolUse func(block ContentBlock)
	// SuppressCategories drops milestone posts whose category is present.
	// e.g. Ross sets {"release-tag": true} because deploy-watcher covers it.
	SuppressCategories map[string]bool
}

// Stream consumes stdout (claude stream-json) line by line: posts debounced
// milestone updates via cfg.Reply, audits every Google-Workspace tool call,
// runs cfg.OnToolUse per tool_use, and returns the final assistant text once
// the result event arrives. Unknown event types and malformed lines are
// skipped without killing the stream.
func Stream(stdout io.Reader, cfg Config) (finalText string, isError bool) {
	debounce := cfg.Debounce
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	scanner := bufio.NewScanner(stdout)
	// stream-json lines can be large (full assistant messages, tool inputs).
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)

	var lastFinalText string
	var lastIsError bool

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt streamEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			slog.Warn("stream_decode_failed", "error", err, "preview", ScrubPreview(string(line), 120))
			continue
		}
		switch evt.Type {
		case "assistant":
			if len(evt.Message) == 0 {
				continue
			}
			var am assistantMessage
			if err := json.Unmarshal(evt.Message, &am); err != nil {
				continue
			}
			for _, block := range am.Content {
				if block.Type != "tool_use" {
					continue
				}
				// Forensic audit for every Google Workspace tool call — shared
				// across all agents (PA agents act on owners' Google accounts,
				// so this is most important there). Inert for non-GWS tools.
				auditGoogleWorkspaceToolUse(block)
				if cfg.OnToolUse != nil {
					cfg.OnToolUse(block)
				}
				if text, category, ok := classifyMilestone(block); ok {
					if cfg.SuppressCategories[category] {
						continue
					}
					if shouldPost(cfg.LastPostedAt, debounce) {
						cfg.Reply(text)
					} else {
						slog.Info("milestone_suppressed_debounce", "text", text)
					}
				}
			}
		case "result":
			lastFinalText = strings.TrimSpace(evt.Result)
			lastIsError = evt.IsError
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("stream_scan_error", "error", err)
	}
	return lastFinalText, lastIsError
}

func shouldPost(lastPostedAt *atomic.Int64, debounce time.Duration) bool {
	if lastPostedAt == nil {
		return true
	}
	last := time.Unix(0, lastPostedAt.Load())
	return time.Since(last) >= debounce
}

// classifyMilestone returns (text, category, ok) for a tool_use block worth
// surfacing. Category lets callers suppress a class (see Config).
func classifyMilestone(block ContentBlock) (text, category string, ok bool) {
	switch block.Name {
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(block.Input, &in); err != nil {
			return "", "", false
		}
		cmd := strings.TrimSpace(in.Command)
		if cmd == "" {
			return "", "", false
		}
		for _, r := range bashMilestoneRules {
			if m := r.pattern.FindStringSubmatch(cmd); m != nil {
				return r.render(m), r.category, true
			}
		}
	case "Write":
		var in struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal(block.Input, &in); err != nil {
			return "", "", false
		}
		if t, ok := classifyWrite(in.FilePath); ok {
			return t, "scaffold", true
		}
	}
	return "", "", false
}

// bashMilestoneRules is the single shared catalog (union of what the three
// agents carried). Ordered: most specific first. Narrow by design — better to
// miss a milestone than spam.
var bashMilestoneRules = []struct {
	category string
	pattern  *regexp.Regexp
	render   func(m []string) string
}{
	{"repo", regexp.MustCompile(`(?m)^\s*git\s+init\b`), func(m []string) string { return "🌱 initialized a new git repo" }},
	{"repo", regexp.MustCompile(`\bgh\s+repo\s+create\s+(\S+)`), func(m []string) string {
		return fmt.Sprintf("📦 created GitHub repo: %s", strings.Trim(m[1], "'\""))
	}},
	{"repo", regexp.MustCompile(`\bgh\s+repo\s+create\b`), func(m []string) string { return "📦 creating a GitHub repo" }},
	{"pr", regexp.MustCompile(`\bgh\s+pr\s+create\b`), func(m []string) string { return "🔀 opened a pull request" }},
	// release-tag: Ross suppresses (deploy-watcher anchor covers the lifecycle).
	{"release-tag", regexp.MustCompile(`\bgit\s+push\b[^|;&]*\bv\d+\.\d+\.\d+\b`), func(m []string) string { return "🚀 pushed release tag (triggers deploy)" }},
	{"release-tag", regexp.MustCompile(`\bgit\s+push\b[^|;&]*--tags\b`), func(m []string) string { return "🚀 pushed tags (triggers deploy)" }},
	{"release", regexp.MustCompile(`\bgh\s+release\s+create\b`), func(m []string) string { return "🚀 cut a GitHub release" }},
	{"deploy", regexp.MustCompile(`\bkubectl\s+apply\b`), func(m []string) string { return "🚢 applying to Kubernetes" }},
	{"deploy", regexp.MustCompile(`\bhelm\s+upgrade\b`), func(m []string) string { return "🚢 helm upgrade in progress" }},
	{"deploy", regexp.MustCompile(`\bfleet\s+apply\b`), func(m []string) string { return "🚢 fleet apply in progress" }},
	{"deploy", regexp.MustCompile(`\bgcloud\s+run\s+deploy\b`), func(m []string) string { return "🚢 deploying to Cloud Run" }},
	{"deploy", regexp.MustCompile(`\bvercel\s+(deploy|--prod)\b`), func(m []string) string { return "🚢 deploying to Vercel" }},
	{"deploy", regexp.MustCompile(`\bwrangler\s+pages\s+deploy\b`), func(m []string) string { return "🚢 deploying to Cloudflare Pages" }},
	{"deploy", regexp.MustCompile(`\bwrangler\s+deploy\b`), func(m []string) string { return "🚢 deploying with Wrangler" }},
	{"migration", regexp.MustCompile(`\b(migrate|migration)\s+(up|apply|run)\b`), func(m []string) string { return "🗄️ applying database migration" }},
	{"migration", regexp.MustCompile(`\balembic\s+upgrade\b`), func(m []string) string { return "🗄️ applying database migration" }},
}

var scaffoldFiles = map[string]bool{
	"package.json": true, "go.mod": true, "Dockerfile": true,
	"Cargo.toml": true, "pyproject.toml": true, "requirements.txt": true,
}

var tfPattern = regexp.MustCompile(`(?i)\.tf$`)

func classifyWrite(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	base := filepath.Base(path)
	if scaffoldFiles[base] || tfPattern.MatchString(base) {
		return fmt.Sprintf("🏗️ scaffolding %s", base), true
	}
	return "", false
}

// --- Secret redaction (shared; all agents scrub log previews) ---

// secretRedactRe matches token-shaped substrings we never want in slog
// previews. Shopify Admin (shpat_) + OAuth user (shpua_) tokens, and an
// env-var-shaped SHOPIFY_ADMIN_TOKEN=value occurrence. Extend as new secret
// shapes enter the fleet.
var secretRedactRe = regexp.MustCompile(`shp(at|ua)_[a-zA-Z0-9_-]{16,}|SHOPIFY_ADMIN_TOKEN=[^\s"]+`)

// Redact scrubs every known secret pattern from s.
func Redact(s string) string {
	if s == "" {
		return s
	}
	return secretRedactRe.ReplaceAllStringFunc(s, func(match string) string {
		const envPrefix = "SHOPIFY_ADMIN_TOKEN="
		if strings.HasPrefix(match, envPrefix) {
			return envPrefix + "[REDACTED]"
		}
		return "[REDACTED_SHOPIFY_TOKEN]"
	})
}

// ScrubPreview redacts secrets THEN truncates to n bytes — redaction happens
// before truncation so we never leave a half-redacted prefix dangling.
func ScrubPreview(s string, n int) string {
	scrubbed := Redact(s)
	if len(scrubbed) <= n {
		return scrubbed
	}
	return scrubbed[:n] + "…"
}

// --- Google Workspace tool-call audit (shared) ---

const gwsToolPrefix = "mcp__google-workspace__"

// auditGoogleWorkspaceToolUse emits a structured slog line for every Google
// Workspace tool_use block, keyed by the spawn's Slack identity env so an abuse
// postmortem is a grep + a Slack search away. Inert for non-GWS tools.
//
// Identity env names are the spawn-site vars all three agents already set
// (SLACK_USER_ID + the ROSS_THREAD_*/ROSS_SESSION_ID trio); they'll be renamed
// when the AGENT_* env cutover lands.
func auditGoogleWorkspaceToolUse(block ContentBlock) {
	if !strings.HasPrefix(block.Name, gwsToolPrefix) {
		return
	}
	attrs := []any{
		"tool", block.Name,
		"slack_user_id", os.Getenv("SLACK_USER_ID"),
		"thread_channel_id", os.Getenv("ROSS_THREAD_CHANNEL_ID"),
		"thread_ts", os.Getenv("ROSS_THREAD_TS"),
		"session_id", os.Getenv("ROSS_SESSION_ID"),
	}
	if block.Name == gwsToolPrefix+"send_gmail_message" {
		var send struct {
			To, Cc, Bcc, Subject string
		}
		if err := json.Unmarshal(block.Input, &send); err == nil {
			attrs = append(attrs, "to", send.To, "cc", send.Cc, "bcc", send.Bcc, "subject", send.Subject)
		}
	}
	attrs = append(attrs, "input_preview", ScrubPreview(string(block.Input), 512))
	slog.Info("gws_tool_call_audit", attrs...)
}
