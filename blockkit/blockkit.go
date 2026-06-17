// Package blockkit converts an agent's textual reply into the right Slack
// MsgOption set: a Block Kit card when a `slack-blocks` fence is present, an
// auto-generated button row when inline `<url|label>` links appear, or plain
// mrkdwn otherwise. Shared by claude-code-ross, claude-code-joanne, and
// claude-code-personal-agent so a single bug fix flows everywhere.
package blockkit

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/bimross/claude-code-core/slackmrkdwn"
	"github.com/slack-go/slack"
)

// Slack imposes a 50-block ceiling per message; over-cap payloads come back
// as "invalid_blocks" with no rendered fallback, so we fall back to plain
// mrkdwn before that happens.
const maxBlocks = 50

// fenceRegexp matches a single fenced ```slack-blocks ... ``` chunk anywhere
// in the message. The `s` flag makes `.` span newlines so we can capture a
// multi-line JSON array; the leading/trailing newline anchors prevent the
// fence markers from matching mid-line. Only the first fence is honoured
// (per the agent-instructions contract: "a single fenced block").
var fenceRegexp = regexp.MustCompile("(?s)(?:^|\n)[\t ]*```slack-blocks[\t ]*\n(.*?)\n[\t ]*```[\t ]*(?:$|\n)")

// linkRegexp captures Slack-mrkdwn link literals: <https://example.com> or
// <https://example.com|label>. The leading `https?://` excludes the other
// shapes Slack uses inside angle brackets (mailto: / tel: links, channel
// mentions <#C…>, user mentions <@U…>, special mentions <!here>) so those
// stay inline.
var linkRegexp = regexp.MustCompile(`<(https?://[^|>\s]+)(?:\|([^>]*))?>`)

// wsRunRegexp collapses runs of two or more spaces or tabs into a single
// space. Used after we strip a link to tidy the prose left behind.
var wsRunRegexp = regexp.MustCompile(`[ \t]{2,}`)

// trailingPunct is the set of punctuation we swallow when it hangs off a
// link. "<url|label>. next sentence" stripped naively would give
// "Live: . next" — the period was attached to the link, not to the
// preceding word, so it should disappear with the link.
const trailingPunct = ".,;:!?"

// buttonTextMax is Slack's per-button text limit. Anything longer trips
// invalid_blocks at post time; truncate with an ellipsis instead so a bare
// URL with a long path still ships.
const buttonTextMax = 75

// buttonsPerActionsBlock is Slack's per-actions-block element ceiling.
// Overflow spills into another actions block.
const buttonsPerActionsBlock = 5

// totalButtonsCap is the hard cap on total auto-generated buttons per
// message. Beyond this the reply is almost certainly a link dump (a
// `gh pr list` output or similar), and inline-mrkdwn rendering is the more
// useful one — fall back rather than carpet-bomb the channel with actions.
const totalButtonsCap = 25

// BuildReplyOpts assembles the MsgOption set for a single Slack post.
// Three paths, in order of precedence:
//
//  1. Explicit ```slack-blocks fence — the agent authored exact blocks;
//     render them verbatim with the surrounding prose as plaintext fallback.
//  2. No fence, but the prose carries inline `<url|label>` links — auto-
//     promote each link to a Block Kit button so they read as clickable
//     affordances instead of underlined runs.
//  3. Neither — plain mrkdwn, same as before this layer existed.
//
// Any failure mode (parse error, over-cap, no links, too many links) falls
// through to the next path; nothing breaks the reply.
func BuildReplyOpts(text string) []slack.MsgOption {
	if payload, fallback, found := ExtractBlockKit(text); found {
		if blocks, ok := renderBlockKit(payload); ok {
			if fallback == "" {
				fallback = "(rich message)"
			}
			return []slack.MsgOption{
				slack.MsgOptionBlocks(blocks...),
				slack.MsgOptionText(slackmrkdwn.ToSlack(fallback), false),
			}
		}
		// Parse failure: ship the stripped fallback (text minus fence)
		// rather than the original text. Original text contains the raw
		// fence markers and JSON, which is exactly the JSON-wall failure
		// we want to avoid. Append a one-line tag so the operator knows
		// the card render didn't happen.
		degraded := strings.TrimSpace(fallback)
		if degraded == "" {
			degraded = "(card render failed)"
		} else {
			degraded += "\n\n_(card render failed, showing plain text)_"
		}
		return []slack.MsgOption{slack.MsgOptionText(slackmrkdwn.ToSlack(degraded), false)}
	}

	// No fence — try auto-promoting inline links. ToSlack runs first so
	// CommonMark `[label](url)` patterns (still emitted by some skills) get
	// normalised to the Slack `<url|label>` form before we match.
	converted := slackmrkdwn.ToSlack(text)
	if blocks, _, ok := convertLinksToButtons(converted); ok {
		// Plaintext fallback keeps the original inline links so push
		// notifications, accessibility tools, and old clients all still
		// see tappable URLs — buttons don't render in any of those.
		return []slack.MsgOption{
			slack.MsgOptionBlocks(blocks...),
			slack.MsgOptionText(converted, false),
		}
	}
	return []slack.MsgOption{slack.MsgOptionText(converted, false)}
}

// ExtractBlockKit pulls the first ```slack-blocks ... ``` fence out of `text`
// and returns the JSON payload, the surrounding prose (with the fence removed
// and surrounding whitespace tidied), and a found flag. It does NOT validate
// the JSON — that's caller's job, so the caller can fall back cleanly on a
// parse failure.
func ExtractBlockKit(text string) (payload string, fallback string, found bool) {
	loc := fenceRegexp.FindStringSubmatchIndex(text)
	if loc == nil {
		return "", text, false
	}
	payload = text[loc[2]:loc[3]]
	fallback = strings.TrimSpace(text[:loc[0]] + "\n" + text[loc[1]:])
	return payload, fallback, true
}

// renderBlockKit attempts to convert the extracted JSON payload into a slice
// of slack.Block ready for MsgOptionBlocks. Accepts two shapes:
//
//  1. Bare array: `[{block}, {block}, ...]`
//  2. Wrapper object: `{"blocks": [{block}, ...]}` — the canonical Block Kit
//     Builder export shape. Common LLM output; the old renderer rejected it
//     and dumped raw JSON into Slack.
//
// Returns ok=false on any failure (bad JSON, neither shape, empty, or over
// the 50-block ceiling) so the caller can fall back to plain mrkdwn without
// partial rendering.
func renderBlockKit(payload string) (blocks []slack.Block, ok bool) {
	trimmed := strings.TrimSpace(payload)
	switch {
	case strings.HasPrefix(trimmed, "["):
		var bs slack.Blocks
		if err := json.Unmarshal([]byte(trimmed), &bs); err != nil {
			return nil, false
		}
		blocks = bs.BlockSet
	case strings.HasPrefix(trimmed, "{"):
		var wrapper struct {
			Blocks slack.Blocks `json:"blocks"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err != nil {
			return nil, false
		}
		blocks = wrapper.Blocks.BlockSet
	default:
		return nil, false
	}
	if len(blocks) == 0 || len(blocks) > maxBlocks {
		return nil, false
	}
	return blocks, true
}

// convertLinksToButtons strips inline `<url|label>` patterns out of `text`
// and returns a Block Kit block slice: at most one section block carrying
// the surviving prose, followed by one or more actions blocks of buttons.
// Returns ok=false when there are no links (let the plain-mrkdwn path take
// it) or when the count blows past the cap (too many buttons looks worse
// than the inline links).
func convertLinksToButtons(text string) (blocks []slack.Block, prose string, ok bool) {
	matches := linkRegexp.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 || len(matches) > totalButtonsCap {
		return nil, text, false
	}

	var buttons []slack.BlockElement
	var sb strings.Builder
	cursor := 0
	for _, m := range matches {
		sb.WriteString(text[cursor:m[0]])
		cursor = m[1]
		// If the link is immediately followed by trailing punctuation AND
		// the char before the link is whitespace (or start-of-string), the
		// punctuation was attached to the link, not to a real word. Eat it
		// with the link so we don't strand "Live: . One-line".
		if cursor < len(text) && strings.IndexByte(trailingPunct, text[cursor]) >= 0 {
			preIsSpace := m[0] == 0 || text[m[0]-1] == ' ' || text[m[0]-1] == '\t'
			if preIsSpace {
				cursor++
			}
		}

		url := text[m[2]:m[3]]
		label := ""
		if m[4] != -1 {
			label = text[m[4]:m[5]]
		}
		label = strings.TrimSpace(label)
		if label == "" {
			label = url
		}
		if r := []rune(label); len(r) > buttonTextMax {
			label = string(r[:buttonTextMax-1]) + "…"
		}

		btn := slack.NewButtonBlockElement("", "", slack.NewTextBlockObject(slack.PlainTextType, label, false, false))
		btn.URL = url
		buttons = append(buttons, btn)
	}
	sb.WriteString(text[cursor:])

	prose = sb.String()
	prose = wsRunRegexp.ReplaceAllString(prose, " ")
	prose = strings.TrimSpace(prose)

	if prose != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, prose, false, false), nil, nil,
		))
	}
	for i := 0; i < len(buttons); i += buttonsPerActionsBlock {
		end := i + buttonsPerActionsBlock
		if end > len(buttons) {
			end = len(buttons)
		}
		blocks = append(blocks, slack.NewActionBlock("", buttons[i:end]...))
	}
	return blocks, prose, true
}
