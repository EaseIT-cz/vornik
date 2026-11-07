package ui

import (
	"os"
	"strings"
	"testing"
)

// GitHub #503 (1): the firewall routes moved off /api/v1/admin/ on 2026-07-03
// (d3714f4d7), but the chunk editor kept posting to the old path, which no
// route serves, so the UI edit form failed from then on. The template must
// post to the registered route.
//
// Found at: review (#495 lane W19-A), observed at production (an operator saving a chunk's policy from the UI)
// Regression test at: unit, the template the UI serves
// Lower because: the page's fetch runs in a browser; this pins the path it calls to the one routes.go registers.
func TestFirewallChunkTemplatePostsToRegisteredRoute_503(t *testing.T) {
	b, err := os.ReadFile("templates/admin_memory_firewall_chunk.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "/api/v1/admin/memory/policy/") {
		t.Fatal("the chunk editor posts to /api/v1/admin/memory/policy/, which no route serves")
	}
	if !strings.Contains(s, "fetch('/api/v1/memory/policy/chunks/'") {
		t.Fatal("the chunk editor does not post to /api/v1/memory/policy/chunks/")
	}
}
