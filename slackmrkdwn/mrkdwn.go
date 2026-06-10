package slackmrkdwn

import (
	"regexp"
	"strings"
)

// Slack's mrkdwn is *not* CommonMark. The model frequently emits GitHub-flavored
// markdown (`**bold**`, `# heading`, `[text](url)`); Slack renders those as
// literal characters. ToSlack rewrites a CommonMark-ish string into the closest
// Slack mrkdwn equivalent before posting.
//
// Conversions:
//   - **x** / __x__   → *x*           (Slack bold is single asterisks)
//   - # x / ## x / ### x → *x*        (Slack has no native headings)
//   - [text](url)     → <url|text>    (Slack link syntax)
//
// Fenced code blocks (```) and inline code (`x`) are left untouched. The
// translation is line-oriented so we can track fenced-code state cheaply.
func ToSlack(in string) string {
	if in == "" {
		return in
	}
	lines := strings.Split(in, "\n")
	inFence := false
	for i, line := range lines {
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lines[i] = translateLine(line)
	}
	return strings.Join(lines, "\n")
}

var fenceRE = regexp.MustCompile("^\\s*```")

func isFenceLine(line string) bool {
	return fenceRE.MatchString(line)
}

var (
	headingRE = regexp.MustCompile(`^(\s*)#{1,6}\s+(.+?)\s*#*\s*$`)
	linkRE    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	// Bold: **x** or __x__. Non-greedy, no newlines, must not be empty.
	// We deliberately don't try to handle nested *** runs perfectly — the
	// common failure mode in Slack is the *literal* asterisks rendering, and
	// this covers >95% of what the model emits.
	boldStarRE  = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	boldUnderRE = regexp.MustCompile(`__([^_\n]+?)__`)
)

func translateLine(line string) string {
	// Heading: the whole body becomes Slack bold, so strip any inner bold
	// markers rather than translating them (would yield `**Big* thing*`).
	if m := headingRE.FindStringSubmatch(line); m != nil {
		body := m[2]
		body = boldStarRE.ReplaceAllString(body, "$1")
		body = boldUnderRE.ReplaceAllString(body, "$1")
		body = linkRE.ReplaceAllString(body, "<$2|$1>")
		return m[1] + "*" + body + "*"
	}
	return applyOutsideInlineCode(line, func(seg string) string {
		seg = boldStarRE.ReplaceAllString(seg, "*$1*")
		seg = boldUnderRE.ReplaceAllString(seg, "*$1*")
		seg = linkRE.ReplaceAllString(seg, "<$2|$1>")
		return seg
	})
}

// applyOutsideInlineCode runs fn on stretches of `line` that are *not* inside
// `inline code` backticks. Inline code is left byte-for-byte intact so we
// don't mangle a snippet like `**not bold**` that the model put in code on
// purpose.
func applyOutsideInlineCode(line string, fn func(string) string) string {
	var b strings.Builder
	b.Grow(len(line))
	i := 0
	for i < len(line) {
		if line[i] == '`' {
			// Find the matching closing backtick on the same line. If there
			// isn't one, treat the rest of the line as code-ish (don't touch).
			j := strings.IndexByte(line[i+1:], '`')
			if j < 0 {
				b.WriteString(line[i:])
				return b.String()
			}
			end := i + 1 + j + 1
			b.WriteString(line[i:end])
			i = end
			continue
		}
		// Read until the next backtick (or EOL) and translate that chunk.
		next := strings.IndexByte(line[i:], '`')
		if next < 0 {
			b.WriteString(fn(line[i:]))
			return b.String()
		}
		b.WriteString(fn(line[i : i+next]))
		i += next
	}
	return b.String()
}
