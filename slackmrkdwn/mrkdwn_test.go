package slackmrkdwn

import "testing"

func TestToSlack(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "hello world", "hello world"},
		{"bold stars", "this is **bold** text", "this is *bold* text"},
		{"bold underscores", "this is __bold__ text", "this is *bold* text"},
		{"multiple bold runs", "**a** and **b**", "*a* and *b*"},
		{"heading h1", "# Heading One", "*Heading One*"},
		{"heading h3 with bold", "### **Big** thing", "*Big thing*"},
		// Note: heading regex strips trailing #'s but the body still gets bold-translated;
		// here we test that body translation runs after heading wrap.
		{"link", "see [Slack](https://slack.com) docs", "see <https://slack.com|Slack> docs"},
		{"link with bold", "[**name**](https://x)", "<https://x|*name*>"},
		{"inline code preserved", "use `**literal**` here", "use `**literal**` here"},
		{"mixed inline code and bold", "`code` and **bold**", "`code` and *bold*"},
		{
			name: "fenced code untouched",
			in:   "before\n```\n**still literal**\n# also literal\n```\nafter **bold**",
			want: "before\n```\n**still literal**\n# also literal\n```\nafter *bold*",
		},
		{
			name: "indented heading",
			in:   "  ## Indented",
			want: "  *Indented*",
		},
		{
			name: "unterminated inline code",
			in:   "weird `start and **bold** after",
			want: "weird `start and **bold** after",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToSlack(tc.in)
			if got != tc.want {
				t.Errorf("ToSlack(%q)\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}
