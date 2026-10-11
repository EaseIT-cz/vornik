package approverdevice

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// GitHub #119 item 6 (2026-10-08): a person on the paired device sees what
// their assistants' memory holds and what broker workflows learned, forgets
// items, and downloads an export that never passes through a model (LLD 22,
// "Memory the user can see", change §3).

type fakeMemoryView struct {
	mu        sync.Mutex
	projects  []MemoryProject
	rows      map[string][]MemoryExportItem // live rows by project
	forgotten map[string]string             // chunk id -> device id
	exports   []string                      // device ids that exported
}

func newFakeMemoryView() *fakeMemoryView {
	v := &fakeMemoryView{
		projects: []MemoryProject{
			{ID: "hermes-memory", Section: MemorySectionAssistants, Holders: []string{"Hermes"}, Exportable: true},
			{ID: "hermes--mail", Namespace: "hermes", Section: MemorySectionBroker},
		},
		rows:      map[string][]MemoryExportItem{},
		forgotten: map[string]string{},
	}
	for i := 0; i < 1000; i++ {
		v.rows["hermes-memory"] = append(v.rows["hermes-memory"], MemoryExportItem{
			ID: fmt.Sprintf("hm-%04d", i), CreatedAt: "2026-10-01T00:00:00Z", SourceName: "companion:hermes:note",
			Origin: "mirrored", Content: fmt.Sprintf("[USER] fact %d", i),
		})
	}
	v.rows["hermes--mail"] = []MemoryExportItem{{ID: "bm-1", SourceName: "mail-digest", Origin: "learned_from_task", Content: "<b>third-party</b> mail text"}}
	return v
}

func (v *fakeMemoryView) listed(p string) bool {
	for _, x := range v.projects {
		if x.ID == p {
			return true
		}
	}
	return false
}

func (v *fakeMemoryView) Projects(context.Context) ([]MemoryProject, error) { return v.projects, nil }

func (v *fakeMemoryView) live(p string) []MemoryExportItem {
	var out []MemoryExportItem
	for _, r := range v.rows[p] {
		if _, gone := v.forgotten[r.ID]; !gone {
			out = append(out, r)
		}
	}
	return out
}

func (v *fakeMemoryView) Page(_ context.Context, p, cursor string, limit int, _ string) (MemoryPageView, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.listed(p) {
		return MemoryPageView{}, ErrMemoryNotListed
	}
	start, _ := strconv.Atoi(cursor)
	rows := v.live(p)
	var out MemoryPageView
	for i := start; i < len(rows) && len(out.Items) < limit; i++ {
		r := rows[i]
		out.Items = append(out.Items, MemoryItemView{ID: r.ID, ShortID: r.ID, Age: "3 days", Source: r.SourceName, Origin: r.Origin, Snippet: r.Content})
	}
	out.Examined = len(out.Items)
	if start+limit < len(rows) {
		out.NextCursor = strconv.Itoa(start + limit)
	}
	return out, nil
}

func (v *fakeMemoryView) Search(ctx context.Context, p, _ string, limit int, d string) (MemoryPageView, error) {
	pg, err := v.Page(ctx, p, "", limit, d)
	pg.Search = true
	return pg, err
}

func (v *fakeMemoryView) Forget(_ context.Context, p, id, device string) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.listed(p) {
		return 0, ErrMemoryNotListed
	}
	for _, r := range v.rows[p] {
		if r.ID == id {
			if _, gone := v.forgotten[id]; gone {
				return 0, nil
			}
			v.forgotten[id] = device
			return 1, nil
		}
	}
	return 0, nil
}

func (v *fakeMemoryView) CountSource(_ context.Context, p, s string) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, r := range v.live(p) {
		if r.SourceName == s {
			n++
		}
	}
	return n, nil
}

func (v *fakeMemoryView) ForgetSource(_ context.Context, p, s, device string) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, r := range v.live(p) {
		if r.SourceName == s {
			v.forgotten[r.ID] = device
			n++
		}
	}
	return n, nil
}

func (v *fakeMemoryView) Export(_ context.Context, p, device string, emit func(MemoryExportItem) error) (MemoryExportCounts, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.exports = append(v.exports, device)
	var c MemoryExportCounts
	for _, r := range v.live(p) {
		c.Examined++
		if err := emit(r); err != nil {
			return c, err
		}
		c.Exported++
	}
	return c, nil
}

func memoryFixture(t *testing.T) (*fixture, *fakeMemoryView, *browser) {
	t.Helper()
	f := newFixture(t)
	v := newFakeMemoryView()
	f.svc.SetMemoryView(v)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	return f, v, phone
}

func TestDeviceMemory_RequiresDevice(t *testing.T) {
	f, v, phone := memoryFixture(t)
	stranger := newBrowser(t, phone.srv)
	for _, path := range []string{"/ui/approve/memory", "/ui/approve/memory/hermes-memory", "/ui/approve/memory/hermes-memory/export?format=json"} {
		res, _ := stranger.do(http.MethodGet, path, nil, nil)
		if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/ui/pair") {
			t.Errorf("GET %s without a device: %d %s", path, res.StatusCode, res.Header.Get("Location"))
		}
	}
	// An operator session cookie or an admin key alone admits nothing.
	res, _ := stranger.do(http.MethodPost, "/ui/approve/memory/hermes-memory/forget", url.Values{"id": {"hm-0001"}},
		map[string]string{"Cookie": "vornik_session=operator", "X-API-Key": "sk-vornik-admin"})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("POST forget with operator credentials only: %d", res.StatusCode)
	}
	if len(v.forgotten) != 0 {
		t.Fatalf("a stranger forgot %v", v.forgotten)
	}
	// The unpaired GET returns to the project page once paired (path, not query).
	res, _ = stranger.do(http.MethodGet, "/ui/approve/memory/hermes-memory", nil, nil)
	if got := res.Header.Get("Location"); got != "/ui/pair?next=%2Fui%2Fapprove%2Fmemory%2Fhermes-memory" {
		t.Errorf("unpaired GET went to %q", got)
	}
	_ = f
}

func TestDeviceMemory_ListsSourceAndAge(t *testing.T) {
	_, _, phone := memoryFixture(t)
	res, body := phone.do(http.MethodGet, "/ui/approve/memory", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "hermes-memory") || !strings.Contains(body, "hermes--mail") {
		t.Fatalf("index: %d %s", res.StatusCode, body)
	}
	res, body = phone.do(http.MethodGet, "/ui/approve/memory/hermes-memory", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("project page: %d", res.StatusCode)
	}
	for _, want := range []string{"3 days ago", "companion:hermes:note", "mirrored", "[USER] fact 0", `name="id" value="hm-0000"`, "/export?format=json", "Older"} {
		if !strings.Contains(body, want) {
			t.Errorf("project page lacks %q", want)
		}
	}
}

func TestDeviceMemory_ForgetIsSameOriginPostOnly(t *testing.T) {
	_, v, phone := memoryFixture(t)
	res, _ := phone.do(http.MethodGet, "/ui/approve/memory/hermes-memory/forget?id=hm-0001", nil, nil)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET forget: %d, want 405", res.StatusCode)
	}
	res, _ = phone.do(http.MethodPost, "/ui/approve/memory/hermes-memory/forget", url.Values{"id": {"hm-0001"}},
		map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST forget: %d, want 403", res.StatusCode)
	}
	res, _ = phone.do(http.MethodPost, "/ui/approve/memory/hermes-memory/forget-source", url.Values{"source": {"companion:hermes:note"}},
		map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST forget-source: %d, want 403", res.StatusCode)
	}
	if len(v.forgotten) != 0 {
		t.Fatalf("forgotten despite refusals: %d", len(v.forgotten))
	}
}

func TestDeviceMemory_ForgetRecordsTheDevice(t *testing.T) {
	f, v, phone := memoryFixture(t)
	res, _ := phone.do(http.MethodPost, "/ui/approve/memory/hermes-memory/forget", url.Values{"id": {"hm-0001"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("forget: %d", res.StatusCode)
	}
	devs, _ := f.svc.ListDevices(context.Background())
	if len(devs) != 1 || v.forgotten["hm-0001"] != devs[0].ID {
		t.Fatalf("forget recorded device %q, want %v", v.forgotten["hm-0001"], devs)
	}
	// Forget by source: a confirmation page first, then the POST.
	_, body := phone.do(http.MethodGet, "/ui/approve/memory/hermes-memory/forget-source?source=companion%3Ahermes%3Anote", nil, nil)
	if !strings.Contains(body, "999 item(s) will be forgotten") {
		t.Fatalf("confirmation lacks the count: %s", body)
	}
	res, _ = phone.do(http.MethodPost, "/ui/approve/memory/hermes-memory/forget-source", url.Values{"source": {"companion:hermes:note"}}, nil)
	if res.StatusCode != http.StatusSeeOther || len(v.forgotten) != 1000 {
		t.Fatalf("forget-source: %d, forgotten %d", res.StatusCode, len(v.forgotten))
	}
}

func TestDeviceMemory_ExportStreamsEveryLiveRow(t *testing.T) {
	f, v, phone := memoryFixture(t)
	v.forgotten["hm-0005"] = "earlier"
	res, body := phone.do(http.MethodGet, "/ui/approve/memory/hermes-memory/export?format=json", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export: %d", res.StatusCode)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "vornik-memory-hermes-memory-") {
		t.Errorf("Content-Disposition %q", cd)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", res.Header.Get("Cache-Control"))
	}
	var out struct {
		Project  string             `json:"project"`
		Items    []MemoryExportItem `json:"items"`
		Examined int                `json:"examined"`
		Exported int                `json:"exported"`
		Complete bool               `json:"complete"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("export does not parse: %v", err)
	}
	if len(out.Items) != 999 || out.Exported != 999 || !out.Complete || out.Project != "hermes-memory" {
		t.Fatalf("export: %d items, exported %d, complete %v", len(out.Items), out.Exported, out.Complete)
	}
	for _, it := range out.Items {
		if it.ID == "hm-0005" {
			t.Fatal("a forgotten row was exported")
		}
	}
	devs, _ := f.svc.ListDevices(context.Background())
	if len(v.exports) != 1 || v.exports[0] != devs[0].ID {
		t.Fatalf("export recorded %v", v.exports)
	}
	res, body = phone.do(http.MethodGet, "/ui/approve/memory/hermes-memory/export?format=md", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "999 exported") {
		t.Fatalf("markdown export: %d", res.StatusCode)
	}
}

func TestDeviceMemory_BrokerMemoryReadAndForgetOnly(t *testing.T) {
	_, v, phone := memoryFixture(t)
	_, body := phone.do(http.MethodGet, "/ui/approve/memory/hermes--mail", nil, nil)
	if !strings.Contains(body, "Forget") || strings.Contains(body, "/export?") {
		t.Fatalf("broker page: Forget present %v, export link present %v", strings.Contains(body, "Forget"), strings.Contains(body, "/export?"))
	}
	if strings.Contains(body, "<b>third-party</b>") {
		t.Fatal("third-party text rendered unescaped")
	}
	res, _ := phone.do(http.MethodGet, "/ui/approve/memory/hermes--mail/export?format=json", nil, nil)
	if res.StatusCode != http.StatusForbidden || len(v.exports) != 0 {
		t.Fatalf("broker export: %d, exports %v", res.StatusCode, v.exports)
	}
}

func TestDeviceMemory_UnlistedProjectIsNotFoundOnEveryRoute(t *testing.T) {
	_, v, phone := memoryFixture(t)
	v.rows["secret-project"] = []MemoryExportItem{{ID: "sp-1", Content: "secret"}}
	for _, rt := range []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodGet, "/ui/approve/memory/secret-project", nil},
		{http.MethodGet, "/ui/approve/memory/secret-project/export?format=json", nil},
		{http.MethodGet, "/ui/approve/memory/secret-project/forget-source?source=x", nil},
		{http.MethodPost, "/ui/approve/memory/secret-project/forget", url.Values{"id": {"sp-1"}}},
		{http.MethodPost, "/ui/approve/memory/secret-project/forget-source", url.Values{"source": {"x"}}},
	} {
		res, body := phone.do(rt.method, rt.path, rt.form, nil)
		if res.StatusCode != http.StatusNotFound || strings.Contains(body, "secret") && !strings.Contains(body, "not a memory") {
			t.Errorf("%s %s: %d", rt.method, rt.path, res.StatusCode)
		}
	}
	if len(v.forgotten) != 0 || len(v.exports) != 0 {
		t.Fatal("an unlisted project was changed or exported")
	}
}

// The process-spawn law: the memory page starts no process and runs no
// archive tool; the export is the handler writing rows.
func TestDeviceMemory_NoProcessSpawn(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "memory.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p == "os/exec" || p == "syscall" || strings.HasSuffix(p, "/internal/spawn") || strings.HasPrefix(p, "archive/") || strings.HasPrefix(p, "compress/") {
			t.Errorf("memory.go imports %s", p)
		}
	}
}
