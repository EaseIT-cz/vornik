package ui

// Collapsed sections still post every field (web-ui-design.md "Progressive
// disclosure across the console — 2026-11-13 (#748)", B1 and its tests).
// Found at: production (the settings page); a browser posts what the form
// renders, so this renders the real form through the handler, collects its
// successful controls the way a browser does (closed <details> or not), posts
// them back, and requires the config to be unchanged.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/registry"
)

// formValues returns what a browser submits for one <form>: named
// inputs (checked ones for checkbox and radio), selects' selected option, and
// textareas. Nothing here looks at <details>, as a browser does not.
func formValues(t *testing.T, body, actionSuffix string) url.Values {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	require.NoError(t, err)
	attr := func(n *html.Node, k string) (string, bool) {
		for _, a := range n.Attr {
			if a.Key == k {
				return a.Val, true
			}
		}
		return "", false
	}
	text := func(n *html.Node) string {
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(c.Data)
			}
		}
		return b.String()
	}
	v := url.Values{}
	// Only the form whose action ends in actionSuffix: a browser submits one
	// form's controls, and the nav carries forms of its own
	// (review-20261113-700d).
	var walk func(n *html.Node, inForm bool)
	walk = func(n *html.Node, inForm bool) {
		if n.Type == html.ElementNode && n.Data == "form" {
			action, _ := attr(n, "action")
			inForm = strings.HasSuffix(action, actionSuffix)
		}
		if n.Type == html.ElementNode && inForm {
			name, named := attr(n, "name")
			_, disabled := attr(n, "disabled")
			if named && !disabled {
				switch n.Data {
				case "input":
					typ, _ := attr(n, "type")
					val, hasVal := attr(n, "value")
					if !hasVal && (typ == "checkbox" || typ == "radio") {
						val = "on"
					}
					_, checked := attr(n, "checked")
					if (typ != "checkbox" && typ != "radio") || checked {
						v.Add(name, val)
					}
				case "textarea":
					v.Add(name, strings.TrimPrefix(text(n), "\n"))
				case "select":
					first, chosen, haveFirst, haveChosen := "", "", false, false
					var opts func(*html.Node)
					opts = func(o *html.Node) {
						for c := o.FirstChild; c != nil; c = c.NextSibling {
							if c.Type == html.ElementNode && c.Data == "option" {
								val, ok := attr(c, "value")
								if !ok {
									val = text(c)
								}
								if !haveFirst {
									first, haveFirst = val, true
								}
								if _, sel := attr(c, "selected"); sel && !haveChosen {
									chosen, haveChosen = val, true
								}
							}
							opts(c)
						}
					}
					opts(n)
					if !haveChosen {
						chosen = first
					}
					v.Add(name, chosen)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inForm)
		}
	}
	walk(doc, false)
	return v
}

func TestSettings748_TheRenderedFormPostsBackUnchanged(t *testing.T) {
	root := writeFormFixture(t)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	existing, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(existing, []byte(`
rate_limit:
  tasks_per_hour: 100
mcp:
  servers:
    - name: scraper
      allowed_tools:
        - web_fetch
    - name: extra
      transport: http
      url: http://extra.local/mcp
      allowed_tools:
        - lookup
`)...), 0o600))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(&reloadingReloader{reg: reg, root: root}),
		WithMCPFormRegistrySource(seededMCPSource()))

	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, `<details class="vk-disc" id="rate-limit">`, "the fixture must exercise a closed section")
	values := formValues(t, body, "/config/form")
	require.Equal(t, "100", values.Get("rateLimit_tasksPerHour"), "a field in a closed section is collected")
	require.Equal(t, "on", values.Get("mcpAllowedTool_scraper_web_fetch"), "a tool box behind a row disclosure is collected")
	// The custom server sits after a registry one in mcp.servers. Custom rows
	// are numbered among themselves (0..n-1), which is what the save walks
	// (mcpCustomCount = n). Numbering them by their position in mcp.servers
	// lost every custom server listed after a registry server, on any save
	// (found 2026-11-13 by this test; web-ui-design.md #748 §D).
	require.Equal(t, "http://extra.local/mcp", values.Get("mcpCustom_0_url"), "a custom row's URL behind its disclosure is collected")

	before, err := os.ReadFile(path)
	require.NoError(t, err)
	rec = httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(values), "form-demo")
	require.Less(t, rec.Code, 400, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "Validation failed")
	after, err := os.ReadFile(path)
	require.NoError(t, err)

	var b, a map[string]any
	require.NoError(t, yaml.Unmarshal(before, &b))
	require.NoError(t, yaml.Unmarshal(after, &a))
	// The save drops an empty list (RemoveIfEmpty), as it did before #748.
	delete(b["permissions"].(map[string]any), "secrets")
	require.Equal(t, b, a, "posting the rendered form back must not change the config")
}
