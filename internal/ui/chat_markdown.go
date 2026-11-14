package ui

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// chatMarkdown renders the web chat's assistant replies (web-chat-design.md
// §2.1): headings, lists, GFM tables and fenced code, as the familiar chat
// UIs do. goldmark's defaults keep it inert: raw HTML in the text is omitted,
// not passed through (html.WithUnsafe is deliberately NOT set), and link
// destinations with a dangerous scheme (javascript:, vbscript:, data: other
// than images, file:) are dropped. Hard wraps: a single newline in a reply
// is a line break, as the model writes it.
var chatMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

// renderChatMarkdown returns a reply as HTML. On a render error it falls
// back to the escaped inline renderer, never to raw text as HTML.
func renderChatMarkdown(text string) template.HTML {
	if text == "" {
		return ""
	}
	var b bytes.Buffer
	if err := chatMarkdown.Convert([]byte(text), &b); err != nil {
		return renderInlineMarkdown(text)
	}
	return template.HTML(b.String()) //nolint:gosec // G203: goldmark output with raw HTML omitted and unsafe links dropped (pinned by TestChatMarkdown_RendersBlocksAndStaysInert_WebChat)
}
