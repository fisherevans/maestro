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
