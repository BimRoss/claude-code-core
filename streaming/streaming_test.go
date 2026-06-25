package streaming

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassifyMilestone_Bash(t *testing.T) {
	cases := []struct {
		cmd      string
		wantText string
		wantCat  string
	}{
		{`git init`, "🌱 initialized a new git repo", "repo"},
		{`gh repo create BimRoss/foo --private`, "📦 created GitHub repo: BimRoss/foo", "repo"},
		{`gh pr create --fill`, "🔀 opened a pull request", "pr"},
		{`git push origin v1.2.3`, "🚀 pushed release tag (triggers deploy)", "release-tag"},
		{`kubectl apply -f x.yaml`, "🚢 applying to Kubernetes", "deploy"},
		{`wrangler pages deploy ./dist`, "🚢 deploying to Cloudflare Pages", "deploy"},
		{`alembic upgrade head`, "🗄️ applying database migration", "migration"},
		{`ls -la`, "", ""},
	}
	for _, c := range cases {
		block := ContentBlock{Type: "tool_use", Name: "Bash", Input: []byte(`{"command":` + jsonStr(c.cmd) + `}`)}
		text, cat, ok := classifyMilestone(block)
		if c.wantText == "" {
			if ok {
				t.Errorf("%q: expected no milestone, got %q", c.cmd, text)
			}
			continue
		}
		if !ok || text != c.wantText || cat != c.wantCat {
			t.Errorf("%q: got (%q,%q,%v) want (%q,%q,true)", c.cmd, text, cat, ok, c.wantText, c.wantCat)
		}
	}
}

func TestClassifyMilestone_Write(t *testing.T) {
	block := ContentBlock{Type: "tool_use", Name: "Write", Input: []byte(`{"file_path":"/ws/go.mod"}`)}
	text, cat, ok := classifyMilestone(block)
	if !ok || cat != "scaffold" || !strings.Contains(text, "go.mod") {
		t.Fatalf("scaffold write: got (%q,%q,%v)", text, cat, ok)
	}
	none := ContentBlock{Type: "tool_use", Name: "Write", Input: []byte(`{"file_path":"/ws/main.go"}`)}
	if _, _, ok := classifyMilestone(none); ok {
		t.Fatal("non-scaffold write should not be a milestone")
	}
}

func TestStream_PostsMilestonesAndReturnsResult(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git init"}}]}}`,
		`{"type":"result","result":"all done","is_error":false}`,
	}, "\n")
	var posts []string
	var lastPosted atomic.Int64
	final, isErr := Stream(strings.NewReader(in), Config{
		Reply:        func(s string) { posts = append(posts, s) },
		LastPostedAt: &lastPosted,
	})
	if final != "all done" || isErr {
		t.Fatalf("final=%q isErr=%v", final, isErr)
	}
	if len(posts) != 1 || posts[0] != "🌱 initialized a new git repo" {
		t.Fatalf("posts=%v", posts)
	}
}

func TestStream_SuppressCategory(t *testing.T) {
	in := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git push origin v1.2.3"}}]}}`
	var posts []string
	var lastPosted atomic.Int64
	Stream(strings.NewReader(in), Config{
		Reply:              func(s string) { posts = append(posts, s) },
		LastPostedAt:       &lastPosted,
		SuppressCategories: map[string]bool{"release-tag": true},
	})
	if len(posts) != 0 {
		t.Fatalf("release-tag should be suppressed, got %v", posts)
	}
}

func TestStream_Debounce(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git init"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gh pr create"}}]}}`,
	}, "\n")
	var posts []string
	var lastPosted atomic.Int64
	lastPosted.Store(time.Now().UnixNano()) // a recent post → debounce active
	Stream(strings.NewReader(in), Config{
		Reply:        func(s string) { posts = append(posts, s) },
		LastPostedAt: &lastPosted,
		Debounce:     time.Hour,
	})
	if len(posts) != 0 {
		t.Fatalf("debounce (1h) should suppress both, got %v", posts)
	}
}

func TestStream_OnToolUseHook(t *testing.T) {
	in := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"vercel --prod"}}]}}`
	var seen []string
	var lastPosted atomic.Int64
	Stream(strings.NewReader(in), Config{
		Reply:        func(string) {},
		LastPostedAt: &lastPosted,
		OnToolUse:    func(b ContentBlock) { seen = append(seen, b.Name) },
	})
	if len(seen) != 1 || seen[0] != "Bash" {
		t.Fatalf("OnToolUse should fire per tool_use, got %v", seen)
	}
}

func TestStream_SkipsMalformedAndUnknown(t *testing.T) {
	in := strings.Join([]string{
		`not json`,
		`{"type":"system","subtype":"init"}`,
		`{"type":"result","result":"ok","is_error":false}`,
	}, "\n")
	var lastPosted atomic.Int64
	final, _ := Stream(strings.NewReader(in), Config{Reply: func(string) {}, LastPostedAt: &lastPosted})
	if final != "ok" {
		t.Fatalf("should skip malformed/unknown and still capture result; final=%q", final)
	}
}

func TestRedactAndScrubPreview(t *testing.T) {
	if got := Redact("token shpat_abcdefghijklmnop1234 here"); strings.Contains(got, "shpat_abcdef") {
		t.Fatalf("shpat_ not redacted: %q", got)
	}
	if got := Redact("SHOPIFY_ADMIN_TOKEN=secretvalue x"); !strings.Contains(got, "SHOPIFY_ADMIN_TOKEN=[REDACTED]") {
		t.Fatalf("env-shaped not redacted: %q", got)
	}
	// Redaction happens before truncation: a secret at the head is gone even if
	// truncation would have cut it.
	long := "shpua_" + strings.Repeat("a", 40) + " " + strings.Repeat("x", 200)
	out := ScrubPreview(long, 50)
	if strings.Contains(out, "shpua_aaaa") {
		t.Fatalf("ScrubPreview leaked a token: %q", out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("ScrubPreview should truncate with ellipsis: %q", out)
	}
}

func TestAudit_InertForNonGWS(t *testing.T) {
	// Should not panic for a non-GWS tool (and emits nothing meaningful).
	auditGoogleWorkspaceToolUse(ContentBlock{Type: "tool_use", Name: "Bash", Input: []byte(`{}`)})
}

func jsonStr(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
