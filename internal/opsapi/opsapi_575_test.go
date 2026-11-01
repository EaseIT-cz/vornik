package opsapi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GitHub #575 (2026-10-28-operations-api-contract-design.md, C3, C4.2, C4.3):
// the served majors, the N/N-1 rule on the real list, and the compatibility
// check against a released copy of each major.

func TestOperationsAPI575_ServedAndRule(t *testing.T) {
	served := Served()
	if !slices.Equal(served, []string{"v1"}) {
		t.Fatalf("Served() = %v, want [v1]", served)
	}
	if err := CheckServed(served); err != nil {
		t.Fatalf("the served majors break the N/N-1 rule: %v", err)
	}
	for _, m := range served {
		doc, ok := Document(m)
		if !ok {
			t.Fatalf("Document(%s) missing", m)
		}
		ops, err := Operations(doc)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		t.Logf("%s: %d operations", m, len(ops))
	}
	if _, ok := Document("v2"); ok {
		t.Error("Document answered for a major that is not served")
	}
}

// The rule is seen to fail on planted lists before it is load-bearing.
func TestOperationsAPI575_CheckServedPlanted(t *testing.T) {
	for _, ok := range [][]string{{"v1"}, {"v1", "v2"}, {"v4", "v5"}} {
		if err := CheckServed(ok); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{}, {"v1", "v3"}, {"v1", "v2", "v3"}, {"vx"}, {"v2", "v1"}} {
		if err := CheckServed(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// The released copy: OPSAPI_RELEASED at release time, else the testdata
// floor, which every served major must have.
func releasedCopy(t *testing.T, major string) []byte {
	t.Helper()
	if p := os.Getenv("OPSAPI_RELEASED"); p != "" {
		if want := os.Getenv("OPSAPI_RELEASED_MAJOR"); want != "" && want != major {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("OPSAPI_RELEASED: %v", err)
		}
		return b
	}
	b, err := os.ReadFile(filepath.Join("testdata", major+".released.yaml"))
	if err != nil {
		t.Fatalf("served major %s has no testdata floor: %v", major, err)
	}
	return b
}

func TestOperationsAPI575_ServedMajorsKeepTheirOperations(t *testing.T) {
	for _, m := range Served() {
		rel := releasedCopy(t, m)
		if rel == nil {
			continue
		}
		cur, _ := Document(m)
		if err := Compatible(cur, rel); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
}

// The compatibility check is seen to fail: a released copy with one more
// operation, and one with one more success status.
func TestOperationsAPI575_CompatiblePlanted(t *testing.T) {
	cur, _ := Document("v1")
	if err := Compatible(cur, cur); err != nil {
		t.Fatalf("identical copies: %v", err)
	}
	extraOp := strings.Replace(string(cur), "paths:\n", "paths:\n  /api/v1/retired:\n    get:\n      responses:\n        '200': {description: x}\n", 1)
	if err := Compatible(cur, []byte(extraOp)); err == nil || !strings.Contains(err.Error(), "GET /api/v1/retired") {
		t.Errorf("a dropped operation passed: %v", err)
	}
	extraStatus := strings.Replace(string(cur), "        '200': {description: The process is alive.}\n", "        '200': {description: The process is alive.}\n        '204': {description: x}\n", 1)
	if extraStatus == string(cur) {
		t.Fatal("the planted status did not apply")
	}
	if err := Compatible(cur, []byte(extraStatus)); err == nil || !strings.Contains(err.Error(), "GET /healthz 204") {
		t.Errorf("a dropped success status passed: %v", err)
	}
}

func TestOperationsAPI575_StructuralParseRefuses(t *testing.T) {
	for name, doc := range map[string]string{
		"not yaml":     "{",
		"openapi 3.0":  "openapi: 3.0.0\ninfo: {title: t, version: '1'}\npaths: {/x: {get: {responses: {'200': {description: x}}}}}\n",
		"no responses": "openapi: 3.1.0\ninfo: {title: t, version: '1'}\npaths: {/x: {get: {summary: x}}}\n",
		"no success":   "openapi: 3.1.0\ninfo: {title: t, version: '1'}\npaths: {/x: {get: {responses: {'404': {description: x}}}}}\n",
		"no paths":     "openapi: 3.1.0\ninfo: {title: t, version: '1'}\n",
	} {
		if _, err := Operations([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every served major is published (C5): an entry in both goreleaser lists, and
// the release step that checks it against the previous release exists (C4.2).
func TestOperationsAPI575_EveryServedMajorIsPublished(t *testing.T) {
	gr, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.enterprise.yaml"))
	if err != nil {
		t.Skipf("no enterprise release config in this tree: %v", err)
	}
	for _, m := range Served() {
		entry := "- glob: ./internal/opsapi/" + m + ".yaml\n      name_template: operations-api-" + m + ".yaml"
		if n := strings.Count(string(gr), entry); n != 2 {
			t.Errorf("%s: %d goreleaser entries, want 2 (release.extra_files and checksum.extra_files)", m, n)
		}
	}
	wf, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wf), "TestOperationsAPI575_ServedMajorsKeepTheirOperations") {
		t.Error("release.yaml does not run the operations API compatibility check")
	}
}
