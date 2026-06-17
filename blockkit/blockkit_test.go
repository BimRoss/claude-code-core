package blockkit

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// One real-world fence with prose on both sides — the failure mode that
// prompted the original ross #253. Exercises that we pull the JSON out
// cleanly and leave both surrounding sentences in the fallback.
func TestExtractBlockKit_FenceBetweenProse(t *testing.T) {
	in := "Here's the carousel:\n\n```slack-blocks\n[{\"type\":\"divider\"}]\n```\n\nLet me know what you think."
	payload, fallback, ok := ExtractBlockKit(in)
	if !ok {
		t.Fatalf("expected fence found, got !ok")
	}
	if payload != `[{"type":"divider"}]` {
		t.Errorf("payload mismatch: %q", payload)
	}
	if !strings.Contains(fallback, "Here's the carousel") || !strings.Contains(fallback, "Let me know what you think.") {
		t.Errorf("fallback should retain both prose halves, got %q", fallback)
	}
	if strings.Contains(fallback, "```") {
		t.Errorf("fallback should not contain fence markers, got %q", fallback)
	}
}

func TestExtractBlockKit_NoFence(t *testing.T) {
	in := "Plain reply, nothing to see here."
	_, fallback, ok := ExtractBlockKit(in)
	if ok {
		t.Fatalf("expected ok=false on plain text")
	}
	if fallback != in {
		t.Errorf("fallback should equal input when no fence, got %q", fallback)
	}
}

// A regular fenced code block (no slack-blocks language tag) must NOT match
// — otherwise we'd hijack every code snippet the model emits.
func TestExtractBlockKit_OrdinaryCodeFenceIgnored(t *testing.T) {
	in := "Look at this:\n```json\n{\"hi\":1}\n```\nDone."
	_, _, ok := ExtractBlockKit(in)
	if ok {
		t.Fatalf("ordinary ```json fence should not match slack-blocks regex")
	}
}

func TestExtractBlockKit_OnlyFirstFenceTaken(t *testing.T) {
	in := "A\n```slack-blocks\n[1]\n```\nB\n```slack-blocks\n[2]\n```\nC"
	payload, fallback, ok := ExtractBlockKit(in)
	if !ok {
		t.Fatalf("expected ok")
	}
	if payload != "[1]" {
		t.Errorf("expected first payload [1], got %q", payload)
	}
	if !strings.Contains(fallback, "```slack-blocks\n[2]") {
		t.Errorf("second fence should remain in fallback, got %q", fallback)
	}
}

func TestRenderBlockKit_ValidArray(t *testing.T) {
	blocks, ok := renderBlockKit(`[{"type":"divider"},{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]`)
	if !ok {
		t.Fatalf("expected ok")
	}
	if len(blocks) != 2 {
		t.Errorf("expected 2 blocks, got %d", len(blocks))
	}
}

func TestRenderBlockKit_InvalidJSON(t *testing.T) {
	if _, ok := renderBlockKit(`[{"type":`); ok {
		t.Fatalf("expected ok=false on bad JSON")
	}
}

// Bare block object (no "blocks" wrapper) — the model forgot the array
// brackets entirely. Still rejected; we can't tell a single-block payload
// apart from a malformed wrapper.
func TestRenderBlockKit_RejectsBareBlockObject(t *testing.T) {
	if _, ok := renderBlockKit(`{"type":"divider"}`); ok {
		t.Fatalf("expected ok=false on bare block object")
	}
}

// Canonical Block Kit Builder export shape: `{"blocks": [...]}`. The
// 2026-06-14 sales-briefing tick emitted this and the old renderer rejected
// it, dumping raw JSON into Slack. This test pins the accepted behavior so
// that bug can't regress.
func TestRenderBlockKit_AcceptsBlocksWrapperObject(t *testing.T) {
	in := `{"blocks":[{"type":"divider"},{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]}`
	blocks, ok := renderBlockKit(in)
	if !ok {
		t.Fatalf("expected ok=true on {blocks: [...]} wrapper")
	}
	if len(blocks) != 2 {
		t.Errorf("expected 2 blocks, got %d", len(blocks))
	}
}

func TestRenderBlockKit_RejectsEmptyBlocksWrapper(t *testing.T) {
	if _, ok := renderBlockKit(`{"blocks":[]}`); ok {
		t.Fatalf("expected ok=false on {blocks: []}")
	}
}

func TestRenderBlockKit_RejectsEmpty(t *testing.T) {
	if _, ok := renderBlockKit(`[]`); ok {
		t.Fatalf("expected ok=false on empty array")
	}
}

func TestRenderBlockKit_RejectsOverCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < maxBlocks+1; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"type":"divider"}`)
	}
	sb.WriteByte(']')
	if _, ok := renderBlockKit(sb.String()); ok {
		t.Fatalf("expected ok=false at %d blocks (cap is %d)", maxBlocks+1, maxBlocks)
	}
}

// BuildReplyOpts integration: a valid fence with surrounding prose returns
// two opts (blocks + plaintext fallback).
func TestBuildReplyOpts_ValidFence(t *testing.T) {
	opts := BuildReplyOpts("intro\n```slack-blocks\n[{\"type\":\"divider\"}]\n```\noutro")
	if len(opts) != 2 {
		t.Fatalf("valid fence should yield 2 opts (blocks + fallback), got %d", len(opts))
	}
}

// A malformed fence must degrade to plain mrkdwn (1 opt) rather than
// stranding the operator with a 500 from the Slack API. The shipped text
// MUST NOT contain the raw fence markers or the broken JSON.
func TestBuildReplyOpts_BadFence(t *testing.T) {
	opts := BuildReplyOpts("intro line\n```slack-blocks\n[ this is not json\n```\noutro line")
	if len(opts) != 1 {
		t.Fatalf("bad fence should fall back to 1 text opt, got %d", len(opts))
	}
	_, fallback, _ := ExtractBlockKit("intro line\n```slack-blocks\n[ this is not json\n```\noutro line")
	if strings.Contains(fallback, "```") || strings.Contains(fallback, "this is not json") {
		t.Errorf("fallback path should strip the fence content, got %q", fallback)
	}
}

// Whole message is just a broken fence (no prose around it). Old code would
// ship the raw fence; new code must still produce something readable.
func TestBuildReplyOpts_BadFenceWholeMessage(t *testing.T) {
	opts := BuildReplyOpts("```slack-blocks\n{not json\n```")
	if len(opts) != 1 {
		t.Fatalf("bad fence should fall back to 1 text opt, got %d", len(opts))
	}
}

func TestBuildReplyOpts_PlainProse(t *testing.T) {
	opts := BuildReplyOpts("Just a regular reply, nothing fancy.")
	if len(opts) != 1 {
		t.Fatalf("plain prose should yield 1 text opt, got %d", len(opts))
	}
}

func TestConvertLinksToButtons_NoLinks(t *testing.T) {
	if _, _, ok := convertLinksToButtons("Just a regular sentence."); ok {
		t.Fatalf("expected ok=false on link-less text")
	}
}

func TestConvertLinksToButtons_OneLabeledLink(t *testing.T) {
	in := "Live: <https://github.com/BimRoss/rancher-admin/pull/443|rancher-admin #443>. One-line RBAC fix."
	blocks, prose, ok := convertLinksToButtons(in)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if len(blocks) != 2 {
		t.Fatalf("expected section+actions (2 blocks), got %d", len(blocks))
	}
	if prose != "Live: One-line RBAC fix." {
		t.Errorf("prose should reflow with the orphan period eaten, got %q", prose)
	}
	action, okA := blocks[1].(*slack.ActionBlock)
	if !okA {
		t.Fatalf("blocks[1] should be ActionBlock, got %T", blocks[1])
	}
	if len(action.Elements.ElementSet) != 1 {
		t.Fatalf("expected 1 button, got %d", len(action.Elements.ElementSet))
	}
	btn, okB := action.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if !okB {
		t.Fatalf("element[0] should be ButtonBlockElement, got %T", action.Elements.ElementSet[0])
	}
	if btn.Text.Text != "rancher-admin #443" {
		t.Errorf("button text should be the label, got %q", btn.Text.Text)
	}
	if btn.URL != "https://github.com/BimRoss/rancher-admin/pull/443" {
		t.Errorf("button URL mismatch, got %q", btn.URL)
	}
	if btn.Style != slack.StyleDefault {
		t.Errorf("button style should be default (light/outlined), got %q", btn.Style)
	}
}

func TestConvertLinksToButtons_BareURL(t *testing.T) {
	blocks, _, ok := convertLinksToButtons("See <https://example.com> for details.")
	if !ok || len(blocks) != 2 {
		t.Fatalf("expected ok+2 blocks, got ok=%v blocks=%d", ok, len(blocks))
	}
	action := blocks[1].(*slack.ActionBlock)
	btn := action.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if btn.Text.Text != "https://example.com" {
		t.Errorf("bare URL should be used as label, got %q", btn.Text.Text)
	}
}

func TestConvertLinksToButtons_LinkOnly(t *testing.T) {
	blocks, prose, ok := convertLinksToButtons("<https://example.com|see>")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if prose != "" {
		t.Errorf("expected empty prose, got %q", prose)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block (actions only), got %d", len(blocks))
	}
	if _, isAction := blocks[0].(*slack.ActionBlock); !isAction {
		t.Fatalf("blocks[0] should be ActionBlock, got %T", blocks[0])
	}
}

func TestConvertLinksToButtons_OverflowAcrossActionsBlocks(t *testing.T) {
	in := "PRs: " +
		"<https://example.com/1|a> " +
		"<https://example.com/2|b> " +
		"<https://example.com/3|c> " +
		"<https://example.com/4|d> " +
		"<https://example.com/5|e> " +
		"<https://example.com/6|f>"
	blocks, _, ok := convertLinksToButtons(in)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks (section + 2 actions), got %d", len(blocks))
	}
	a1 := blocks[1].(*slack.ActionBlock)
	a2 := blocks[2].(*slack.ActionBlock)
	if len(a1.Elements.ElementSet) != 5 || len(a2.Elements.ElementSet) != 1 {
		t.Errorf("expected 5+1 button split, got %d+%d",
			len(a1.Elements.ElementSet), len(a2.Elements.ElementSet))
	}
}

func TestConvertLinksToButtons_OverCap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < totalButtonsCap+1; i++ {
		sb.WriteString("<https://example.com/x|x> ")
	}
	if _, _, ok := convertLinksToButtons(sb.String()); ok {
		t.Fatalf("expected ok=false past the total-buttons cap")
	}
}

func TestConvertLinksToButtons_LongLabelTruncated(t *testing.T) {
	longLabel := strings.Repeat("x", buttonTextMax+20)
	in := "see <https://example.com|" + longLabel + ">"
	blocks, _, ok := convertLinksToButtons(in)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	btn := blocks[1].(*slack.ActionBlock).Elements.ElementSet[0].(*slack.ButtonBlockElement)
	runes := []rune(btn.Text.Text)
	if len(runes) != buttonTextMax {
		t.Errorf("expected truncated to %d runes, got %d", buttonTextMax, len(runes))
	}
	if runes[len(runes)-1] != '…' {
		t.Errorf("expected trailing ellipsis, got %q", btn.Text.Text)
	}
}

func TestConvertLinksToButtons_IgnoresNonHTTPAngleBrackets(t *testing.T) {
	cases := []string{
		"email <mailto:grant@bimross.com>",
		"hey <@U0APBT3364D>",
		"in <#C0B5W8L5744>",
		"<!here> heads up",
		"call <tel:+15555551212>",
	}
	for _, c := range cases {
		if _, _, ok := convertLinksToButtons(c); ok {
			t.Errorf("expected ok=false for non-http angle brackets: %q", c)
		}
	}
}

func TestBuildReplyOpts_AutoLink(t *testing.T) {
	opts := BuildReplyOpts("Live: <https://example.com|see PR>. fix shipped.")
	if len(opts) != 2 {
		t.Fatalf("expected 2 opts (blocks + fallback), got %d", len(opts))
	}
}
