package graph

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #284 (knowledge-graph LLD §3.3a): 1,760 production chunks were
// quarantined because the mention surface — a slice of the chunk at the
// model's byte offsets — cut a multi-byte character, which Postgres refuses.
// These are the unit-level companions of repotest.RunGraphPipelineStorableTextSuite,
// which reproduces the production error on the Postgres lane.

func TestStorableText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Škoda Auto", "Škoda Auto"},
		{"\x80\x94 Škoda", "� Škoda"}, // the census's leading continuation bytes
		{"Praha \xf0\x9f", "Praha �"}, // a lead byte cut at the end
		{"Jan\x00 Novák", "Jan Novák"},
		{"\x00\x00", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := storableText(c.in); got != c.want {
			t.Errorf("storableText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStorableJSON(t *testing.T) {
	out, ok := storableJSON(json.RawMessage(`{"note\u0000":"sídlo\u0000 firmy","list":["a\u0000",{"k":"v\u0000"}],"n":3}`))
	if !ok {
		t.Fatalf("a decodable document must be kept")
	}
	if strings.Contains(string(out), `\u0000`) || !utf8.Valid(out) {
		t.Errorf("NUL survived: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-encoded document does not decode: %v", err)
	}
	if got["note"] != "sídlo firmy" || got["n"] != float64(3) {
		t.Errorf("content changed beyond NUL removal: %s", out)
	}

	// Raw invalid bytes in a RawMessage (it is copied undecoded).
	out, ok = storableJSON(json.RawMessage("{\"k\":\"a\x80b\"}"))
	if !ok || !utf8.Valid(out) {
		t.Errorf("raw invalid bytes not repaired: ok=%v %q", ok, out)
	}

	// Undecodable: dropped, reported.
	if out, ok := storableJSON(json.RawMessage(`{"n":1e400}`)); ok || out != nil {
		t.Errorf("undecodable document must be dropped: ok=%v %s", ok, out)
	}
	if out, ok := storableJSON(nil); !ok || out != nil {
		t.Errorf("no document stays no document: ok=%v %s", ok, out)
	}
}

func TestValidateCandidates_dropsASpanThatCutsACharacter(t *testing.T) {
	content := "Smlouva — Škoda Auto → Praha 🙂 hotovo."
	dash := strings.Index(content, "—")
	skoda := strings.Index(content, "Škoda Auto")
	praha := strings.Index(content, "Praha")
	emoji := strings.Index(content, "🙂")

	got := validateCandidates([]Candidate{
		{Type: "VENDOR", Name: "Škoda Auto", CharStart: dash + 1, CharEnd: skoda + len("Škoda Auto")},
		{Type: "LOCATION", Name: "Praha", CharStart: praha, CharEnd: emoji + 2},
		{Type: "VENDOR", Name: "Škoda", CharStart: skoda + 1, CharEnd: skoda + len("Škoda")}, // inside "Š"
		{Type: "LOCATION", Name: "Praha", CharStart: praha, CharEnd: praha + len("Praha"), Surface: "wrong"},
	}, content)
	if len(got) != 4 {
		t.Fatalf("every entity is kept when its span is dropped, got %d", len(got))
	}
	for i := 0; i < 3; i++ {
		if got[i].CharStart != 0 || got[i].CharEnd != 0 || got[i].Surface != "" {
			t.Errorf("candidate %d: a span cutting a character must be dropped, got %+v", i, got[i])
		}
	}
	if got[3].Surface != "Praha" || got[3].CharEnd != praha+len("Praha") {
		t.Errorf("a span on character boundaries is kept with the chunk's text: %+v", got[3])
	}
}

func TestValidateCandidates_repairsModelText(t *testing.T) {
	got := validateCandidates([]Candidate{
		{Type: "PERSON", Name: "Jan\x00 Novák", Subtype: "ře\x00ditel", Surface: "Ja\x00n"},
		{Type: "OTHER", Name: "\x00 \x00"}, // empty after repair: dropped
		{Type: "VENDOR", Name: "A\x80B"},
	}, "Jan Novák")
	if len(got) != 2 {
		t.Fatalf("want 2 candidates (NUL-only name dropped), got %+v", got)
	}
	if got[0].Name != "Jan Novák" || got[0].Subtype != "ředitel" || got[0].Surface != "Jan" {
		t.Errorf("model text not repaired: %+v", got[0])
	}
	if got[1].Name != "A�B" {
		t.Errorf("invalid UTF-8 not replaced: %q", got[1].Name)
	}
}

// The resolver's merge_aliases and the relationship stage's edge properties
// are model text written by the pipeline itself.
func TestPipeline_repairsAliasesAndEdgeProperties(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-284", ProjectID: "proj-1", Content: "Vadim chose PostgreSQL 16 for the new ledger build."}
	extractReply := `[
	  {"type":"PERSON","name":"Vadim","char_start":0,"char_end":5},
	  {"type":"TECHNOLOGY","name":"PostgreSQL 16","char_start":12,"char_end":25}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"match","match_id":"kent-existing","merge_aliases":["Va\u0000dim","\u0000"]},
	  {"candidate_id":"cand-1","decision":"new"}
	]`
	relReply := `[
	  {"from":"kent-existing","to":"kent-new-technology-1","predicate":"DEPENDS_ON","evidence":"Vadim chose PostgreSQL 16","properties":{"why\u0000":"led\u0000ger"}},
	  {"from":"kent-new-technology-1","to":"kent-existing","predicate":"RELATES_TO","evidence":"Vadim chose PostgreSQL 16","properties":{"n":1e400}}
	]`
	valReply := `[{"id":"prop-0","score":0.95},{"id":"prop-1","score":0.95}]`

	p, entRepo, edgeRepo, _ := newPipeline(t, extractReply, resolveReply, relReply, valReply)
	// A real entity of the project (GitHub #419: a match_id is verified).
	entRepo.byID = map[string]*persistence.KnowledgeEntity{
		"kent-existing": {ID: "kent-existing", ProjectID: "proj-1", Type: "PERSON", CanonicalName: "Vadim"},
	}
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if len(entRepo.addAliased) != 1 || entRepo.addAliased[0].Alias != "Vadim" {
		t.Errorf("aliases must be repaired and an empty one skipped: %+v", entRepo.addAliased)
	}
	if len(edgeRepo.upserts) != 2 {
		t.Fatalf("both edges are kept, got %d", len(edgeRepo.upserts))
	}
	for _, e := range edgeRepo.upserts {
		switch e.Predicate {
		case "DEPENDS_ON":
			if string(e.Properties) != `{"why":"ledger"}` {
				t.Errorf("edge properties not repaired: %s", e.Properties)
			}
		case "RELATES_TO":
			if e.Properties != nil {
				t.Errorf("undecodable properties must be dropped: %s", e.Properties)
			}
		}
	}
	if m.EdgePropertiesDropped != 1 {
		t.Errorf("EdgePropertiesDropped = %d, want 1", m.EdgePropertiesDropped)
	}
}
