package ui

// #748 rollout batch 2 (web-ui-design.md "Progressive disclosure across the
// console", G). Found at: production; the memory page's Operate tab rendered
// 500 epoch rows (45,714 visible characters) and its page-size selector showed
// no option selected for the 500 default. The test drives the real handler.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func renderMemoryOperate(t *testing.T, n int, query string) string {
	t.Helper()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	epochs := make([]*persistence.CorpusEpoch, 0, n)
	// Newest first, as ListEpochs returns them; the newest is active, so every
	// older epoch is a rollback target.
	for i := 0; i < n; i++ {
		epochs = append(epochs, &persistence.CorpusEpoch{
			ID: fmt.Sprintf("epoch_%03d", n-i), ProjectID: "p1",
			CreatedAt: base.Add(time.Duration(n-i) * time.Hour), IsActive: i == 0,
		})
	}
	srv := NewServer(
		WithCorpusEpochRepository(&uiStubEpochRepo{listEpochsFn: func(context.Context, string, int) ([]*persistence.CorpusEpoch, error) { return epochs, nil }}),
		WithMemoryQuarantineRepository(&uiStubQuarantineRepo{}),
		// The eviction panel, and its selector, render only with an evictor.
		WithMemoryEvictor(&stubMemoryEvictor{}),
	)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, httptest.NewRequest(http.MethodGet, "/memory/p1?tab=operate"+query, nil), "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	return rec.Body.String()
}

// Since #748 M the older rows are not in the page: they load when the
// disclosure opens (disclosure_748_lazy_epochs_test.go covers the fragment).
func TestBatch2_748_OlderEpochsSitBehindADisclosure(t *testing.T) {
	body := renderMemoryOperate(t, 30, "")
	rows := regexp.MustCompile(`title="epoch_\d{3}"`)
	if got := len(rows.FindAllString(body, -1)); got != 20 {
		t.Fatalf("the page renders the newest 20 epochs: got %d", got)
	}
	if !strings.Contains(body, ">10 older epochs<") {
		t.Error("the disclosure names how many older epochs it holds")
	}
	if got := strings.Count(body, `action="/ui/memory/p1/rollback"`); got != 19 {
		t.Errorf("rollback forms in the page: got %d, want 19 (the newest 20 less the active one)", got)
	}
	if fewer := renderMemoryOperate(t, 5, ""); strings.Contains(fewer, "older epochs<") {
		t.Error("with 20 or fewer epochs there is nothing to fold")
	}
	if none := renderMemoryOperate(t, 0, ""); !strings.Contains(none, "No epochs yet") || strings.Contains(none, "older epochs<") {
		t.Error("with no epochs the first table keeps its empty row and there is no disclosure")
	}
}

func TestBatch2_748_TheEpochsSelectorShowsTheLimitInForce(t *testing.T) {
	for _, c := range []struct{ query, want string }{{"", "500"}, {"&epochs_limit=10", "10"}} {
		body := renderMemoryOperate(t, 3, c.query)
		i := strings.Index(body, `name="epochs_limit"`)
		if i < 0 {
			t.Fatal("no epochs_limit selector")
		}
		end := strings.Index(body[i:], "</select>")
		if end < 0 {
			t.Fatal("the epochs_limit selector has no closing </select>")
		}
		sel := body[i : i+end]
		if !regexp.MustCompile(`<option value="` + c.want + `"[^>]*selected`).MatchString(sel) {
			t.Errorf("epochs limit %q: the %s option must show as selected:\n%s", c.query, c.want, sel)
		}
	}
	// Only the Epochs selector offers 500; every other page-size selector on
	// the page keeps the shared 10 to 100 (review-20261113-f0bf M1: the
	// check must examine each one, and fail if one is missing). Anchored on
	// the <select> elements: the selectors' forms carry each other's limits
	// as hidden inputs.
	body := renderMemoryOperate(t, 3, "")
	others := 0
	for _, loc := range regexp.MustCompile(`<select id="page-size-([a-z_]+)"`).FindAllStringSubmatchIndex(body, -1) {
		param := body[loc[2]:loc[3]]
		end := strings.Index(body[loc[0]:], "</select>")
		if end < 0 {
			t.Fatalf("selector %s has no closing </select>", param)
		}
		if param == "epochs_limit" {
			continue
		}
		others++
		if strings.Contains(body[loc[0]:loc[0]+end], `value="500"`) {
			t.Errorf("selector %s offers 500; only the Epochs selector may", param)
		}
	}
	if others < 3 {
		t.Fatalf("examined %d other page-size selectors on the Operate tab, want the 3 (quarantine, rollbacks, eviction)", others)
	}
}

func TestBatch2_748_ProjectDetailDisclosuresAreShared(t *testing.T) {
	raw, err := os.ReadFile("templates/project_detail.html")
	if err != nil {
		t.Fatal(err)
	}
	markup := regexp.MustCompile(`(?s)<script\b.*?</script>|<!--.*?-->|\{\{/\*.*?\*/\}\}`).ReplaceAllString(string(raw), "")
	tags := regexp.MustCompile(`<details\b[^>]*>`).FindAllString(markup, -1)
	if len(tags) == 0 {
		t.Fatal("no <details> examined")
	}
	classOf := regexp.MustCompile(`class="([^"]*)"`)
	for _, tag := range tags {
		if m := classOf.FindStringSubmatch(tag); m == nil || (m[1] != "vk-disc" && m[1] != "vk-disc vk-disc-row") {
			t.Errorf("%s: a disclosure carries exactly the component's classes", tag)
		}
	}
	for _, sum := range regexp.MustCompile(`<summary\b[^>]*>`).FindAllString(markup, -1) {
		if m := classOf.FindStringSubmatch(sum); m == nil || strings.Fields(m[1])[0] != "vk-disc-sum" {
			t.Errorf("%s: a summary starts with vk-disc-sum", sum)
		}
	}
	if strings.Contains(markup, "panel-ref") {
		t.Error("project_detail.html still carries panel-ref")
	}
	if !strings.Contains(markup, `<details class="vk-disc" open>`) {
		t.Error("Workflow wiring stays open by default")
	}
	t.Logf("%d disclosures examined", len(tags))
}
