package subjectstore

import (
	"context"
	"slices"
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/secrets"
)

// Every table the subject axis may link to must have a collector, or an Art 15
// export would silently omit that table's content while still listing the item
// — the subject would be told data exists and shown nothing, with no indication
// that the gap is ours rather than theirs.
//
// CollectItems refuses an uncollectable table at runtime too; this fails at
// build time instead, on the commit that widens the closed set.
func TestEveryLinkableTableHasACollector(t *testing.T) {
	var missing []string
	for _, name := range datasubject.LinkableTables() {
		if _, ok := collectorSpecs[datasubject.LinkableTable(name)]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("linkable tables with no collector spec: %v", missing)
	}
}

// A collector must not name a table outside the closed set: the table name is
// interpolated into SQL, so the closed set is also the injection boundary.
func TestNoCollectorForAnUnlinkableTable(t *testing.T) {
	allowed := map[string]bool{}
	for _, name := range datasubject.LinkableTables() {
		allowed[name] = true
	}
	for table := range collectorSpecs {
		if !allowed[string(table)] {
			t.Errorf("collector spec for %q, which is not a linkable table", table)
		}
	}
}

// Each spec must actually be able to read something, and name where the data
// came from — Art 14(2)(f) entitles a subject to the source.
func TestCollectorSpecsAreWellFormed(t *testing.T) {
	for table, spec := range collectorSpecs {
		if spec.idCol == "" {
			t.Errorf("%s: no id column", table)
		}
		if len(spec.contentCols) == 0 {
			t.Errorf("%s: no content columns — the export would list the item with nothing in it", table)
		}
		if len(spec.originNote) < 15 {
			t.Errorf("%s: origin note too thin to answer Art 14(2)(f): %q", table, spec.originNote)
		}
	}
}

// audit 2026-10-16 (GitHub #344, slice 5d-E, design 2026-07-29-art17-redact-
// and-reembed §15.11.4): the transcript collectors left out the author, the
// tool-call arguments, the hallucination signals and the message metadata. The
// three JSON columns drop their empty defaults; history does not, so the
// shipped session export is unchanged.
func TestTranscriptCollectorsExportEveryTextColumn(t *testing.T) {
	want := map[datasubject.LinkableTable][]string{
		datasubject.TableChatAuditLog: {"user_message", "response", "user_id", "tool_calls_json", "hallucination_signals_json"},
		datasubject.TableTaskMessages: {"content", "author_id", "metadata"},
	}
	for table, cols := range want {
		spec := collectorSpecs[table]
		for _, c := range cols {
			if !slices.Contains(spec.contentCols, c) {
				t.Errorf("%s: export omits column %s (has %v)", table, c, spec.contentCols)
			}
		}
		for _, j := range spec.jsonCols {
			if !slices.Contains(spec.contentCols, j) {
				t.Errorf("%s: jsonCols names %s, which is not a content column", table, j)
			}
		}
	}
	jsonWant := map[datasubject.LinkableTable][]string{
		datasubject.TableChatAuditLog: {"tool_calls_json", "hallucination_signals_json"},
		datasubject.TableTaskMessages: {"metadata"},
	}
	for table, cols := range jsonWant {
		if !slices.Equal(collectorSpecs[table].jsonCols, cols) {
			t.Errorf("%s: jsonCols = %v, want %v", table, collectorSpecs[table].jsonCols, cols)
		}
	}
	for table, spec := range collectorSpecs {
		if _, ok := jsonWant[table]; !ok && len(spec.jsonCols) != 0 {
			t.Errorf("%s: jsonCols = %v, want none (the shipped export of this table must not change)", table, spec.jsonCols)
		}
	}
}

// security review 2026-10-16 (#344, 5d-E): a credential under a secret-named
// JSON key has no value shape the detector knows, so the key name decides.
func TestIsSecretKeyName(t *testing.T) {
	for _, k := range []string{"password", "Password", "api_key", "apiKey", "X-Auth-Token",
		"Authorization", "client_secret", "accessToken", "session_id", "privateKey", "cookie",
		// review c2d9 F1: plurals and compounds.
		"tokens", "secrets", "passwords", "cookies", "apiKeys", "api_keys", "access_tokens",
		"accessTokens", "refresh_token", "credentials"} {
		if !isSecretKeyName(k) {
			t.Errorf("isSecretKeyName(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"author_id", "max_tokens", "maxTokens", "prompt_tokens", "total_tokens",
		"reply_to_message_id", "chat_id", "address", "note", "passage", "keyboard", "name"} {
		if isSecretKeyName(k) {
			t.Errorf("isSecretKeyName(%q) = true, want false", k)
		}
	}
}

func TestRedactExportJSON(t *testing.T) {
	// Tool arguments keep their structure, so the walk is pinned there.
	in := `[{"name":"t","args":"{\"to\":\"bob@example.test\",\"password\":\"opaque-value-1234\",` +
		`\"token\":{\"inner\":\"x\"},\"note\":\"key AKIAABCDEFGHIJKLMNOP here\",\"n\":7}"}]`
	got := redactExportJSON("tool_calls_json", in)
	for _, gone := range []string{"opaque-value-1234", "AKIAABCDEFGHIJKLMNOP", `"inner"`} {
		if strings.Contains(got, gone) {
			t.Errorf("redactExportJSON kept %q: %s", gone, got)
		}
	}
	for _, kept := range []string{"bob@example.test", `\"n\":7`, "[REDACTED:secret_field]", "[REDACTED:aws_access_key]"} {
		if !strings.Contains(got, kept) {
			t.Errorf("redactExportJSON lost %q: %s", kept, got)
		}
	}
}

// security review 2026-10-16 on b720b4ad2: parser differentials. Every case
// stores a secret the export must not carry in ANY form, and the output must
// say something was withheld or redacted. Fail closed: near-JSON, a second
// value, trailing garbage, an unknown shape or a duplicate key withholds the
// whole column value.
func TestRedactExportJSON_FailsClosed(t *testing.T) {
	cases := []struct {
		name, col, in, secret string
	}{
		{"trailing comma", "tool_calls_json", `[{"name":"x","args":"{\"password\":\"SECRETV01\"}",}]`, "SECRETV01"},
		{"single quotes", "tool_calls_json", `[{'name':'x','password':'SECRETV02'}]`, "SECRETV02"},
		{"NaN", "tool_calls_json", `[{"name":"x","args":NaN,"password":"SECRETV03"}]`, "SECRETV03"},
		{"truncated array", "tool_calls_json", `[{"name":"x","args":"{\"password\":\"SECRETV04\"}"}`, "SECRETV04"},
		{"invalid UTF-8", "metadata", "{\"reason\":\"\xff SECRETV05\"}", "SECRETV05"},
		{"second value", "metadata", `{"reason":"ok"} {"password":"SECRETV06"}`, "SECRETV06"},
		{"second value after array", "tool_calls_json", `[] {"password":"SECRETV07"}`, "SECRETV07"},
		{"trailing garbage", "metadata", `{"reason":"ok"} garbage SECRETV08`, "SECRETV08"},
		{"wrapped calls", "tool_calls_json", `{"calls":[{"name":"x","result":"SECRETV09"}]}`, "SECRETV09"},
		{"nested result under unknown key", "tool_calls_json", `[{"name":"x","args":"{}","extra":{"result":"SECRETV10"}}]`, "SECRETV10"},
		{"non-object call", "tool_calls_json", `["SECRETV11"]`, "SECRETV11"},
		{"duplicate key", "metadata", `{"reason":"SECRETV12","reason":"ok"}`, "SECRETV12"},
		{"duplicate key nested", "metadata", `{"reason":{"a":"SECRETV13","a":"ok"}}`, "SECRETV13"},
		{"duplicate key inside args", "tool_calls_json", `[{"name":"x","args":"{\"note\":\"SECRETV14\",\"note\":\"x\"}"}]`, "SECRETV14"},
		{"escaped secret name", "metadata", `{"reason":{"password":"SECRETV15"}}`, "SECRETV15"},
		{"escaped secret name in args", "tool_calls_json", `[{"name":"x","args":"{\"pass\\u0077ord\":\"SECRETV16\"}"}]`, "SECRETV16"},
		{"number under secret key", "metadata", `{"reason":{"otp":987654321}}`, "987654321"},
		{"secret in a key", "metadata", `{"reason":{"AKIAABCDEFGHIJKLMNOP":1}}`, "AKIAABCDEFGHIJKLMNOP"},
		{"secret in an allowed value", "metadata", `{"status":"AKIAABCDEFGHIJKLMNOP"}`, "AKIAABCDEFGHIJKLMNOP"},
		{"truncated args string", "tool_calls_json", `[{"name":"x","args":"{\"password\":\"SECRETV17\",\"to\":\"bo"}]`, "SECRETV17"},
		{"args with secret name", "tool_calls_json", `[{"name":"x","args":"{\"api_key\":\"SECRETV18\"}"}]`, "SECRETV18"},
		{"unknown signal key", "hallucination_signals_json", `[{"detector":"x","secret_stuff":"SECRETV19"}]`, "SECRETV19"},
		{"signals not a list", "hallucination_signals_json", `{"sentence":"SECRETV20"}`, "SECRETV20"},
		{"caller key name is data", "metadata", `{"SECRETV21@mail.test":"x"}`, "SECRETV21"},
		{"tool error", "tool_calls_json", `[{"name":"x","error":"SECRETV22"}]`, "SECRETV22"},
		{"non-string name", "tool_calls_json", `[{"name":{"password":"SECRETV23"}}]`, "SECRETV23"},
		{"unknown column", "content", `{"a":"SECRETV24"}`, "SECRETV24"},
		// review c2d9 F1/F3: a plural credential key in args, and structure
		// nested under an allowed metadata key (the allowlist vouches for the
		// key's scalar text, not for a shape a future writer puts under it).
		{"plural secret key in args", "tool_calls_json", `[{"name":"x","args":"{\"access_tokens\":[\"SECRETV25\"]}"}]`, "SECRETV25"},
		{"structure under allowed key", "metadata", `{"reason":{"detail":"SECRETV26@mail.test"}}`, "SECRETV26"},
		{"list under allowed key", "metadata", `{"choice":["SECRETV27"]}`, "SECRETV27"},
		// review 38b6 F3: the writer stores args as a string; any other type
		// is not its shape and is withheld, not walked.
		{"non-string args object", "tool_calls_json", `[{"name":"x","args":{"note":"SECRETV28"}}]`, "SECRETV28"},
		{"non-string args list", "tool_calls_json", `[{"name":"x","args":["SECRETV29"]}]`, "SECRETV29"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactExportJSON(c.col, c.in)
			if strings.Contains(got, c.secret) {
				t.Fatalf("export carries %q: %q", c.secret, got)
			}
			if got == c.in {
				t.Fatalf("export returned the stored bytes unchanged: %q", got)
			}
			if !strings.Contains(got, "[WITHHELD:") && !strings.Contains(got, "[REDACTED:") {
				t.Fatalf("export does not say anything was withheld or redacted: %q", got)
			}
		})
	}
}

// review f0fa F1: result and error are strings (chatAuditToolCall); any other
// type is not the writer's shape and withholds the whole column, as a
// non-string name does.
func TestRedactExportJSON_NonStringToolResultIsAShapeViolation(t *testing.T) {
	for _, in := range []string{
		`[{"name":"x","result":{"a":"b"}}]`,
		`[{"name":"x","error":42}]`,
		`[{"name":"x","result":false}]`,
	} {
		if got := redactExportJSON("tool_calls_json", in); got != withheldShape {
			t.Errorf("redactExportJSON(%s) = %q, want %q", in, got, withheldShape)
		}
	}
}

// review c2d9 F5: without a working detector, rule 1 cannot run, so the
// column is withheld whole rather than exported half-scanned.
func TestRedactExportJSON_NoDetectorFailsClosed(t *testing.T) {
	prev := exportDetectorFn
	exportDetectorFn = func() secrets.Detector { return nil }
	t.Cleanup(func() { exportDetectorFn = prev })
	got := redactExportJSON("metadata", `{"reason":"AKIAABCDEFGHIJKLMNOP"}`)
	if got != withheldNoScanner {
		t.Fatalf("export without a detector = %q, want %q", got, withheldNoScanner)
	}
}

// The writers' real shapes still export their non-secret parts.
func TestRedactExportJSON_KeepsTheWritersShapes(t *testing.T) {
	got := redactExportJSON("tool_calls_json",
		`[{"name":"send_email","args":"{\"to\":\"bob@x.test\"}","result":"sent to bob"}]`)
	for _, want := range []string{"send_email", "bob@x.test", withheldToolResult} {
		if !strings.Contains(got, want) {
			t.Errorf("tool call export lost %q: %s", want, got)
		}
	}
	if strings.Contains(got, "sent to bob") {
		t.Errorf("tool result exported: %s", got)
	}
	got = redactExportJSON("hallucination_signals_json",
		`[{"detector":"url","severity":"warn","claim_type":"url","claim_value":"https://a.test","sentence":"see a.test","recorded_at":"2026-10-16T00:00:00Z"}]`)
	if !strings.Contains(got, "see a.test") {
		t.Errorf("signal export lost its sentence: %s", got)
	}
	got = redactExportJSON("metadata", `{"choice":"yes","chat_id":-100,"custom":"x"}`)
	if !strings.Contains(got, `"choice":"yes"`) || !strings.Contains(got, `"`+withheldMetadataCount+`":2`) ||
		strings.Contains(got, "chat_id") || strings.Contains(got, "custom") {
		t.Errorf("metadata export = %s", got)
	}
}

func TestEmptyJSONDefaultIsNotContent(t *testing.T) {
	for _, v := range []string{"[]", "{}", "null", " [ ] ", "", "  "} {
		if !emptyJSONValue(v) {
			t.Errorf("emptyJSONValue(%q) = false, want true", v)
		}
	}
	for _, v := range []string{`[{"a":1}]`, `{"k":"v"}`, "x", `"null"`} {
		if emptyJSONValue(v) {
			t.Errorf("emptyJSONValue(%q) = true, want false", v)
		}
	}
}

// --- erasure execution (increment 5, slice 5b) ---

// DeleteRow interpolates the table and id column into SQL, so both must come
// from the same closed map the collector uses. Reusing collectorSpecs rather than
// adding a parallel eraserSpecs map means TestEveryLinkableTableHasACollector
// already guards erasure coverage too — one source of truth, one coverage test.
func TestDeleteRow_UsesTheClosedCollectorSpecSet(t *testing.T) {
	for _, name := range datasubject.LinkableTables() {
		spec, ok := collectorSpecs[datasubject.LinkableTable(name)]
		if !ok {
			t.Errorf("%s has no spec, so DeleteRow could not erase it", name)
			continue
		}
		if strings.TrimSpace(spec.idCol) == "" {
			t.Errorf("%s has no id column, so DeleteRow has no WHERE clause to target", name)
		}
	}
}

// Artifacts must never be deleted as a plain row: extracted_documents has no FK
// on source_artifact_id and project_memory_chunks.artifact_id is ON DELETE SET
// NULL, so a row delete orphans the extraction and leaves the derived embedding
// in the vector store. The cascade in internal/erasure is the only correct path,
// and this is the defence-in-depth guard in case a caller routes it wrongly.
func TestDeleteRow_RefusesArtifactsToProtectTheCascade(t *testing.T) {
	r := &Repository{}
	err := r.DeleteRow(context.Background(), datasubject.TableArtifacts, "art-1")
	if err == nil {
		t.Fatal("DeleteRow must refuse artifacts — they require the cascade")
	}
	if !strings.Contains(err.Error(), "cascade") {
		t.Errorf("the refusal should point at the cascade, got %v", err)
	}
}

func TestDeleteRow_RefusesATableOutsideTheClosedSet(t *testing.T) {
	r := &Repository{}
	if err := r.DeleteRow(context.Background(), datasubject.LinkableTable("tool_audit_log"), "r1"); err == nil {
		t.Fatal("DeleteRow must refuse a table with no spec rather than build SQL from it")
	}
}

func TestDeleteRow_RefusesAnEmptyRowID(t *testing.T) {
	r := &Repository{}
	if err := r.DeleteRow(context.Background(), datasubject.TableChatAuditLog, "  "); err == nil {
		t.Fatal("an empty row id must be refused — the WHERE clause would be meaningless")
	}
}

// The accountability chain must not record a hash of a document nobody kept.
//
// Found by the 5b code review chasing a different question: report_hash was
// written to the ledger unconditionally while the report itself was saved only if
// the operator happened to pass --out. A fingerprint of an unsaved document proves
// nothing, and it is the evidence Art 5(2) accountability rests on — so the
// report is stored WITH the hash, in one call, or not at all.
func TestSaveRequestReport_RequiresBothTheReportAndItsHash(t *testing.T) {
	r := &Repository{}
	ctx := context.Background()
	if err := r.SaveRequestReport(ctx, "req-1", "", "sha256:abc"); err == nil {
		t.Error("a hash with no report body must be refused — it would attest to nothing")
	}
	if err := r.SaveRequestReport(ctx, "req-1", `{"plan":{}}`, ""); err == nil {
		t.Error("a report with no hash must be refused — nothing would pin it")
	}
	if err := r.SaveRequestReport(ctx, "", `{"plan":{}}`, "sha256:abc"); err == nil {
		t.Error("an empty request id must be refused")
	}
}
