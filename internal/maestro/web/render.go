package web

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// md is a single goldmark instance reused across requests. Default config
// plus GFM table/strikethrough/autolink and a hard-break setting because
// note bodies tend to be terminal-style line-broken text.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

// renderMarkdown converts arbitrary text (markdown-ish) to safe HTML. Raw
// HTML in the input is escaped, not rendered, so it's safe to pass into
// templates as template.HTML.
func renderMarkdown(s string) template.HTML {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := md.Convert([]byte(s), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return template.HTML(buf.String())
}

// reportField is one key/value pair extracted from a structured note body.
// The implementer report shape (STATUS:, SUMMARY:, FILES:, COMMIT:, DEFERRED:,
// CONCERNS:, NOTES:) and the merge sub-agent's report shape (STATUS:,
// SUMMARY:, REVIEW_FINDINGS:, VERIFY_NOTES:, MERGE_COMMIT:, SMOKE_TAIL:)
// both parse this way.
type reportField struct {
	Key   string
	Value string
	// HTML is the markdown-rendered value, set by parseReportNote.
	HTML template.HTML
}

// recognizedReportKeys is the set of top-level keys parseReportNote will
// treat as field boundaries. Unknown keys (lines that start with TEXT:
// but aren't in this set) get appended to the current field's body, so we
// don't accidentally fragment user prose like "Note: I tried X first".
var recognizedReportKeys = map[string]bool{
	"STATUS":          true,
	"SUMMARY":         true,
	"FILES":           true,
	"COMMIT":          true,
	"DEFERRED":        true,
	"CONCERNS":        true,
	"NOTES":           true,
	"REVIEW_FINDINGS": true,
	"VERIFY_NOTES":    true,
	"MERGE_COMMIT":    true,
	"SMOKE_TAIL":      true,
	"TAGS":            true,
}

// parseReportNote tries to split a structured "KEY: value" note into fields.
// Returns nil if it doesn't recognize at least two known keys - in that case
// the caller should render the note body as plain markdown.
func parseReportNote(content string) []reportField {
	lines := strings.Split(content, "\n")
	var fields []reportField
	var cur *reportField

	flush := func() {
		if cur == nil {
			return
		}
		cur.Value = strings.TrimSpace(cur.Value)
		cur.HTML = renderMarkdown(cur.Value)
		fields = append(fields, *cur)
		cur = nil
	}

	for _, line := range lines {
		if k, v, ok := splitReportLine(line); ok {
			flush()
			cur = &reportField{Key: k, Value: v}
			continue
		}
		if cur != nil {
			if cur.Value != "" {
				cur.Value += "\n"
			}
			cur.Value += line
		}
	}
	flush()

	if len(fields) < 2 {
		return nil
	}
	return fields
}

// searchMatch is one hit from a text search: where it landed on the task and
// a short snippet showing the matched substring in context. Used by the web
// UI's search results page so the user can see why each row matched.
type searchMatch struct {
	Field   string        // "label" | "description" | "summary" | "note (<source>/<type>)" | "tag"
	Snippet template.HTML // pre-rendered HTML with the match wrapped in <mark>
}

// matchesFor returns up to maxPerField snippets per source field. Always
// returns at least one match if anything contained the needle; otherwise nil.
// Tag matches are returned as Field="tag", Snippet=the tag itself.
func matchesFor(label, description, summary string, tags []string, notes []note, queryText string, queryTags []string, maxSnippet int) []searchMatch {
	var out []searchMatch
	q := strings.TrimSpace(queryText)
	if q != "" {
		if m := buildSnippet(label, q, maxSnippet); m != "" {
			out = append(out, searchMatch{Field: "label", Snippet: m})
		}
		if m := buildSnippet(description, q, maxSnippet); m != "" {
			out = append(out, searchMatch{Field: "description", Snippet: m})
		}
		if m := buildSnippet(summary, q, maxSnippet); m != "" {
			out = append(out, searchMatch{Field: "summary", Snippet: m})
		}
		for _, n := range notes {
			if m := buildSnippet(n.Content, q, maxSnippet); m != "" {
				label := "note"
				if n.Source != "" || n.Type != "" {
					parts := []string{}
					if n.Source != "" {
						parts = append(parts, n.Source)
					}
					if n.Type != "" {
						parts = append(parts, n.Type)
					}
					label = "note (" + strings.Join(parts, "/") + ")"
				}
				out = append(out, searchMatch{Field: label, Snippet: m})
			}
		}
	}
	tagSet := make(map[string]bool, len(queryTags))
	for _, t := range queryTags {
		tagSet[strings.TrimSpace(t)] = true
	}
	for _, t := range tags {
		if tagSet[t] {
			out = append(out, searchMatch{Field: "tag", Snippet: template.HTML(template.HTMLEscapeString(t))})
		}
	}
	return out
}

// note is a thin shim so matchesFor doesn't import maestro just for the
// Note shape. handlers.go converts maestro.Note to this on the way in.
type note struct {
	Source  string
	Type    string
	Content string
}

// buildSnippet finds needle in haystack (case-insensitive) and returns an
// HTML-escaped excerpt with the matched substring wrapped in <mark>. Returns
// "" if no match. The window is approximately `window` chars centered on
// the first match; leading/trailing ellipses indicate truncation.
func buildSnippet(haystack, needle string, window int) template.HTML {
	if needle == "" || haystack == "" {
		return ""
	}
	lowerHay := strings.ToLower(haystack)
	lowerNeedle := strings.ToLower(needle)
	i := strings.Index(lowerHay, lowerNeedle)
	if i < 0 {
		return ""
	}
	nl := len(needle)
	half := window / 2
	start := i - half
	if start < 0 {
		start = 0
	}
	end := i + nl + half
	if end > len(haystack) {
		end = len(haystack)
	}
	// Expand to word boundaries when cheap.
	for start > 0 && haystack[start] != ' ' && haystack[start] != '\n' && i-start < half+10 {
		start--
	}
	for end < len(haystack) && haystack[end-1] != ' ' && haystack[end-1] != '\n' && end-i-nl < half+10 {
		end++
	}
	pre := haystack[start:i]
	hit := haystack[i : i+nl]
	post := haystack[i+nl : end]
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(template.HTMLEscapeString(pre))
	b.WriteString(`<mark>`)
	b.WriteString(template.HTMLEscapeString(hit))
	b.WriteString(`</mark>`)
	b.WriteString(template.HTMLEscapeString(post))
	if end < len(haystack) {
		b.WriteString("…")
	}
	// Collapse runs of whitespace to make snippets compact.
	out := strings.Join(strings.Fields(b.String()), " ")
	return template.HTML(out)
}

// splitReportLine returns (KEY, value, ok=true) when a line looks like
// `KEY: rest of value`, the KEY is in recognizedReportKeys, and there's no
// leading whitespace (so indented "Note:" inside prose doesn't trigger).
func splitReportLine(line string) (string, string, bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' {
		return "", "", false
	}
	i := strings.IndexByte(line, ':')
	if i <= 0 || i > 24 {
		return "", "", false
	}
	key := strings.ToUpper(strings.TrimSpace(line[:i]))
	if !recognizedReportKeys[key] {
		return "", "", false
	}
	return key, strings.TrimSpace(line[i+1:]), true
}
