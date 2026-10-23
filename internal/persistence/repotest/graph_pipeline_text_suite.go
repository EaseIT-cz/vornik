package repotest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #284 (knowledge-graph LLD §3.3a): the KG worker failed 1,760
// production chunks at the MENTION insert with `pq: invalid byte sequence for
// encoding "UTF8"`, quarantining each after three attempts. The chunk text was
// clean; the mention surface was not — validateCandidates sliced the chunk at
// the model's byte offsets, which land inside a multi-byte character on Czech
// and typographic text. Postgres refuses the slice; SQLite stores the broken
// bytes. A JSON "\u0000" from the model is the other way model output becomes
// unstorable (NUL is refused by Postgres TEXT and JSONB).
//
// This suite drives the real graph.Pipeline against the driver's real
// repositories with scripted model replies and requires the chunk to succeed
// with every stored string valid UTF-8 and NUL-free. It runs on both drivers
// because they disagree about invalid text: only Postgres fails the chunk,
// only SQLite stores the damage.

// GraphPipelineRepos are the three repositories the pipeline writes through.
type GraphPipelineRepos struct {
	Entities persistence.KnowledgeEntityRepository
	Edges    persistence.KnowledgeEdgeRepository
	Mentions persistence.EntityMentionRepository
}

// scriptedChat answers each call with reply(messages). It is the whole model
// for one pipeline stage.
type scriptedChat struct {
	reply func(msgs []chat.Message) string
}

func (s *scriptedChat) Complete(_ context.Context, msgs []chat.Message) (*chat.ChatResponse, error) {
	resp := &chat.ChatResponse{Model: "scripted"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: s.reply(msgs)}, FinishReason: "stop"})
	resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens = 10, 5, 15
	return resp, nil
}

func (s *scriptedChat) CompleteWithTools(ctx context.Context, msgs []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return s.Complete(ctx, msgs)
}

func (s *scriptedChat) CompleteWithToolsStream(ctx context.Context, msgs []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.Complete(ctx, msgs)
}

func (s *scriptedChat) Model() string            { return "scripted" }
func (s *scriptedChat) SetMetrics(*chat.Metrics) {}

func fixedReply(s string) *scriptedChat {
	return &scriptedChat{reply: func([]chat.Message) string { return s }}
}

// storable is the property every string the pipeline persists must have.
func storable(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// kgCand is one scripted extractor candidate.
type kgCand struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	CharStart int    `json:"char_start,omitempty"`
	CharEnd   int    `json:"char_end,omitempty"`
	Subtype   string `json:"subtype,omitempty"`
}

// kgEdge is the one edge the scripted relationship model proposes, between
// two entities named by canonical name (their ids exist only in the prompt).
type kgEdge struct {
	From, To, Evidence string
	Properties         map[string]string
}

// relationshipReply answers the relationship stage with e, resolving the
// canonical names to the ids the pipeline put in the prompt.
func relationshipReply(t *testing.T, e kgEdge) *scriptedChat {
	return &scriptedChat{reply: func(msgs []chat.Message) string {
		user := msgs[len(msgs)-1].Content
		start := strings.Index(user, "ENTITIES:\n") + len("ENTITIES:\n")
		end := strings.Index(user, "\n\nCHUNK:")
		var ents []graph.ResolvedEntity
		if start < len("ENTITIES:\n") || end < start {
			t.Errorf("relationship prompt has no ENTITIES block")
			return "[]"
		}
		if err := json.Unmarshal([]byte(user[start:end]), &ents); err != nil {
			t.Errorf("relationship prompt: %v", err)
			return "[]"
		}
		ids := map[string]string{}
		for _, en := range ents {
			ids[en.CanonicalName] = en.ID
		}
		b, _ := json.Marshal([]map[string]any{{
			"from": ids[e.From], "to": ids[e.To],
			"predicate": persistence.PredicateLocatedAt,
			"evidence":  e.Evidence, "properties": e.Properties,
		}})
		return string(b)
	}}
}

// runKGChunk seeds a chunk and runs the real pipeline over it.
func runKGChunk(ctx context.Context, t *testing.T, repos GraphPipelineRepos, seed SeedChunk,
	project, chunkID, content string, cands []kgCand, resolveReply string, edge kgEdge,
) (*graph.PipelineMetrics, error) {
	t.Helper()
	if err := seed(ctx, chunkID, project, content, true, time.Now()); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	extractJSON, err := json.Marshal(cands)
	if err != nil {
		t.Fatal(err)
	}
	p := &graph.Pipeline{
		Extractor: graph.NewExtractor(fixedReply(string(extractJSON)), ""),
		Resolver:  graph.NewResolver(fixedReply(resolveReply), "", repos.Entities, nil),
		Relations: graph.NewRelationshipExtractor(relationshipReply(t, edge), ""),
		Validator: graph.NewValidator(fixedReply(`[{"id":"prop-0","score":0.95,"reason":"explicit"}]`), ""),
		Entities:  repos.Entities, Edges: repos.Edges, Mentions: repos.Mentions,
	}
	return p.RunChunk(ctx, graph.ChunkInput{ID: chunkID, ProjectID: project, Content: content})
}

// RunGraphPipelineStorableTextSuite pins GitHub #284 on one driver.
func RunGraphPipelineStorableTextSuite(t *testing.T, repos GraphPipelineRepos, seed SeedChunk) {
	t.Helper()
	t.Run("offsets inside a multi-byte character", func(t *testing.T) {
		runCutSpanCase(t, repos, seed)
	})
	t.Run("NUL in a name, subtype, alias and edge property", func(t *testing.T) {
		runNULCase(t, repos, seed)
	})
}

// The production shape: valid names, offsets that cut a character. Before
// the fix this failed on Postgres with exactly the production error
// (`insert mention for entity ... invalid byte sequence for encoding "UTF8"`).
func runCutSpanCase(t *testing.T, repos GraphPipelineRepos, seed SeedChunk) {
	ctx := context.Background()
	project, chunkID := uniqueID("proj"), uniqueID("chunk")
	// "—" is e2 80 94, "Š" c5 a0, "→" e2 86 92, the emoji f0 9f 99 82.
	content := "Smlouva — Škoda Auto → Praha 🙂 hotovo."
	dash := strings.Index(content, "—")
	skodaEnd := strings.Index(content, "Škoda Auto") + len("Škoda Auto")
	praha := strings.Index(content, "Praha")
	emoji := strings.Index(content, "🙂")

	cands := []kgCand{
		// Start on the second byte of "—": a continuation byte first, the
		// census's commonest shape (0x80 / 0x94 / 0x92).
		{Type: persistence.EntityTypeVendor, Name: "Škoda Auto", CharStart: dash + 1, CharEnd: skodaEnd},
		// End inside the emoji: a lead byte last (0xe2 / 0xf0).
		{Type: persistence.EntityTypeLocation, Name: "Praha", CharStart: praha, CharEnd: emoji + 2},
	}
	resolve := `[{"candidate_id":"cand-0","decision":"new"},{"candidate_id":"cand-1","decision":"new"}]`
	edge := kgEdge{From: "Škoda Auto", To: "Praha", Evidence: "Škoda Auto → Praha",
		Properties: map[string]string{"role": "sídlo"}}

	m, err := runKGChunk(ctx, t, repos, seed, project, chunkID, content, cands, resolve, edge)
	if err != nil {
		t.Fatalf("RunChunk failed the chunk (#284: a model offset inside a multi-byte "+
			"character must not fail extraction): %v", err)
	}
	mentions, err := repos.Mentions.ListByChunk(ctx, chunkID)
	if err != nil {
		t.Fatalf("ListByChunk: %v", err)
	}
	if len(mentions) != 2 {
		t.Errorf("mentions = %d, want 2: the entity is kept when its span is dropped", len(mentions))
	}
	for _, mn := range mentions {
		if !storable(mn.Surface) {
			t.Errorf("mention %s stored surface %q: not valid UTF-8", mn.EntityID, mn.Surface)
		}
		// Dropped, not stored and not guessed (§3.3: the row is the record).
		if mn.CharEnd != nil || mn.Surface != "" {
			t.Errorf("a span cutting a character must be dropped: start=%d surface=%q", mn.CharStart, mn.Surface)
		}
	}
	if m.MentionsWithoutSpan != 2 {
		t.Errorf("MentionsWithoutSpan = %d, want 2", m.MentionsWithoutSpan)
	}
	if m.EdgesUpserted != 1 {
		t.Errorf("EdgesUpserted = %d, want 1: the chunk's relationships are still extracted", m.EdgesUpserted)
	}
}

// Model output that decodes to NUL: refused by Postgres TEXT and JSONB.
func runNULCase(t *testing.T, repos GraphPipelineRepos, seed SeedChunk) {
	ctx := context.Background()
	project, chunkID := uniqueID("proj"), uniqueID("chunk")
	content := "Jan Novák, ředitel, sídlí v Praha; Škoda a.s. také."

	// An existing entity the resolver matches, so a model-written alias
	// reaches AddAlias.
	existing := &persistence.KnowledgeEntity{
		ProjectID: project, Type: persistence.EntityTypeVendor, CanonicalName: "Skoda",
	}
	if err := repos.Entities.Insert(ctx, existing); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	cands := []kgCand{
		{Type: persistence.EntityTypePerson, Name: "Jan\u0000 Novák", Subtype: "ře\u0000ditel"},
		{Type: persistence.EntityTypeLocation, Name: "Praha"},
		// Nothing but NUL and space: empty after repair, so dropped.
		{Type: persistence.EntityTypeOther, Name: "\u0000 \u0000"},
		// Far enough from "Skoda" (Levenshtein > 3, not a substring) that
		// the resolver's string short-circuit does not fire and the model's
		// match decision, with its alias, is what lands.
		{Type: persistence.EntityTypeVendor, Name: "Škoda a.s."},
	}
	// Resolver ids are positional over the candidates that SURVIVE
	// validation: with the NUL-only name dropped the vendor is cand-2. Before
	// the fix it survived as cand-2 and the vendor was cand-3; decisions for
	// both keep the reply meaningful either way.
	resolve := `[
	  {"candidate_id":"cand-0","decision":"new"},
	  {"candidate_id":"cand-1","decision":"new"},
	  {"candidate_id":"cand-2","decision":"match","match_id":"` + existing.ID + `","merge_aliases":["Škoda\u0000 a.s."]},
	  {"candidate_id":"cand-3","decision":"match","match_id":"` + existing.ID + `","merge_aliases":["Škoda\u0000 a.s."]}
	]`
	edge := kgEdge{From: "Jan Novák", To: "Praha", Evidence: "sídlí v Praha",
		Properties: map[string]string{"note\u0000": "sídlo\u0000 firmy"}}

	if _, err := runKGChunk(ctx, t, repos, seed, project, chunkID, content, cands, resolve, edge); err != nil {
		t.Fatalf("RunChunk failed the chunk (#284: a NUL from the model must not fail extraction): %v", err)
	}

	jan, err := repos.Entities.GetByCanonical(ctx, project, persistence.EntityTypePerson, "Jan Novák")
	if err != nil {
		t.Fatalf("entity not stored under its repaired name %q: %v", "Jan Novák", err)
	}
	if !storable(string(jan.Properties)) || strings.Contains(string(jan.Properties), `\u0000`) ||
		!strings.Contains(string(jan.Properties), "ředitel") {
		t.Errorf("subtype not stored repaired: %s", jan.Properties)
	}
	if _, err := repos.Entities.GetByCanonical(ctx, project, persistence.EntityTypeOther, ""); err == nil {
		t.Errorf("a name empty after repair must be dropped, not stored")
	}
	mentions, err := repos.Mentions.ListByChunk(ctx, chunkID)
	if err != nil {
		t.Fatalf("ListByChunk: %v", err)
	}
	if len(mentions) != 3 {
		t.Errorf("mentions = %d, want 3 (the NUL-only name is dropped)", len(mentions))
	}

	seeded, err := repos.Entities.Get(ctx, existing.ID)
	if err != nil {
		t.Fatalf("Get seeded entity: %v", err)
	}
	if strings.Contains(string(seeded.Aliases), `\u0000`) || !strings.Contains(string(seeded.Aliases), "Škoda a.s.") {
		t.Errorf("alias not stored repaired: %s", seeded.Aliases)
	}

	edges, err := repos.Edges.List(ctx, persistence.KnowledgeEdgeFilter{ProjectID: project})
	if err != nil {
		t.Fatalf("List edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(edges))
	}
	props := string(edges[0].Properties)
	if !storable(props) || strings.Contains(props, `\u0000`) || !strings.Contains(props, "sídlo firmy") {
		t.Errorf("edge properties not stored repaired: %s", props)
	}
}
