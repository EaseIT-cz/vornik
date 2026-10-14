//go:build e2e_hermes

package hermes

// H3f — forgetting reaches Vornik (design 24, "Forgetting reaches Vornik, and
// the user can see what is kept (0.8.0)", E2E: the Hermes lane, H3 extended).
//
// The fact is saved through Hermes's OWN memory tool, so the plugin's mirror
// writes it to Vornik with its identity token; then Hermes is told to forget
// it, which removes the entry from Hermes's file and, through the provider's
// on_memory_write, refutes the Vornik copy. H3's dentist cannot be reused: H3
// stores it with vornik_remember, the model's own note, which carries no
// token and which nothing the model can do makes Vornik forget (design item
// 6). So H3f uses a fact of its own (As built 0.8.0 records the deviation).

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	promptH3fDel = "Forget my physiotherapist: remove that entry from your memory with your memory tool."
	promptH3fAsk = "Who is my physiotherapist? Check your Vornik long-term memory."
)

var mirrorTokenRe = regexp.MustCompile(`⟦vm:[0-9a-f]{16}⟧`)

// h3fScripts drive the stub model mode for H3f.
func h3fScripts() []Script {
	echo := func(last string) string { return "Here is what Vornik returned: " + last }
	return []Script{
		{Marker: "Use your own memory tool", Steps: []ScriptStep{{ToolSuffix: "memory", Args: map[string]any{
			"action": "add", "target": "user", "content": "The user's physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic."}}}, Final: "Saved."},
		{Marker: "Forget my physiotherapist", Steps: []ScriptStep{{ToolSuffix: "memory", Args: map[string]any{
			"action": "remove", "target": "user", "old_text": h3fFact}}}, Final: "Forgotten."},
		{Marker: "Who is my physiotherapist", Steps: []ScriptStep{{ToolSuffix: "vornik_recall", Args: map[string]any{"query": "physiotherapist"}}}, FinalFrom: echo},
	}
}

// memoryChunks reads the memory project's chunks mentioning text ("" for
// all), straight from the lane's Postgres (refuted rows included: recall
// hides them).
func memoryChunks(t *testing.T, text string) []memChunk {
	t.Helper()
	q := fmt.Sprintf(`SELECT id || chr(31) || coalesce(validation_status,'') || chr(31) || replace(content, chr(10), ' ')
FROM project_memory_chunks WHERE project_id = 'assistant-memory' AND content LIKE '%%%s%%' ORDER BY created_at`, text)
	out, err := exec.Command("podman", "exec", pgContainerName(), "psql", "-U", "vornik", "-d", "vornik", "-tAc", q).Output()
	if err != nil {
		t.Fatalf("read memory chunks: %v", err)
	}
	var rows []memChunk
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Split(line, "\x1f"); len(f) == 3 {
			rows = append(rows, memChunk{f[0], f[1], f[2]})
		}
	}
	return rows
}

// memoryChunkRaw reads one chunk's content byte for byte (base64 through
// psql), for the identical re-add that reinstates it.
func memoryChunkRaw(t *testing.T, id string) string {
	t.Helper()
	q := fmt.Sprintf(`SELECT encode(convert_to(content, 'UTF8'), 'base64') FROM project_memory_chunks
WHERE project_id = 'assistant-memory' AND id = '%s'`, id)
	out, err := exec.Command("podman", "exec", pgContainerName(), "psql", "-U", "vornik", "-d", "vornik", "-tAc", q).Output()
	if err != nil {
		t.Fatalf("read chunk %s: %v", id, err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(out)), ""))
	if err != nil || len(raw) == 0 {
		t.Fatalf("read chunk %s: %v (%q)", id, err, tail(string(out), 200))
	}
	return string(raw)
}

// hermesMemoryFiles reads Hermes's two memory files; one that does not
// exist is absent from the map.
func hermesMemoryFiles(s *stack) map[string][]byte {
	files := map[string][]byte{}
	for _, name := range []string{"USER.md", "MEMORY.md"} {
		if raw, err := os.ReadFile(filepath.Join(s.hermesHome, "memories", name)); err == nil {
			files[name] = raw
		}
	}
	return files
}

// h3fLaneOps are h3fResetAttempt's effects on the lane: the daemon's own
// tools through the Hermes memory key (memory_correct to remove, an
// identical remember to reinstate), and Hermes's files on the host (the
// container has exited and the home is chowned back by runHermes).
func h3fLaneOps(t *testing.T, s *stack) h3fResetOps {
	return h3fResetOps{
		chunks: func() []memChunk { return memoryChunks(t, "") },
		refute: func(id string) error {
			if text, isErr := mcpCall(t, s.apiURL, s.memoryKey, "memory_correct",
				map[string]any{"chunk_ids": []string{id}, "reason": "forget_command"}); isErr {
				return fmt.Errorf("memory_correct: %s", text)
			}
			return nil
		},
		files: func() map[string][]byte { return hermesMemoryFiles(s) },
		restoreFiles: func(files map[string][]byte) error {
			for _, name := range []string{"USER.md", "MEMORY.md"} {
				path := filepath.Join(s.hermesHome, "memories", name)
				raw, ok := files[name]
				if !ok {
					if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
						return err
					}
					continue
				}
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					return err
				}
			}
			return nil
		},
		reinstate: func(content string) error {
			text, isErr := mcpCall(t, s.apiURL, s.memoryKey, "remember", map[string]any{"content": content})
			if isErr {
				return fmt.Errorf("remember: %s", text)
			}
			t.Logf("H3f-forget: identical re-add answered %s", tail(text, 300))
			return nil
		},
		logf: t.Logf,
	}
}

// hermesEntry returns Hermes's own stored entry mentioning text, and its
// target ("user" for USER.md, "memory" for MEMORY.md).
func hermesEntry(t *testing.T, s *stack, text string) (entry, target string) {
	t.Helper()
	for file, tgt := range map[string]string{"USER.md": "user", "MEMORY.md": "memory"} {
		raw, err := os.ReadFile(filepath.Join(s.hermesHome, "memories", file))
		if err != nil {
			continue
		}
		for _, e := range strings.Split(string(raw), "\n§\n") {
			if strings.Contains(e, text) {
				return strings.TrimSpace(e), tgt
			}
		}
	}
	return "", ""
}

// expectedToken is the plugin's mirror_token, computed independently from
// Hermes's stored entry: the parity the forget path depends on.
func expectedToken(target, entry string) string {
	sum := sha256.Sum256([]byte(target + "\n" + strings.Join(strings.Fields(entry), " ")))
	return "⟦vm:" + hex.EncodeToString(sum[:])[:16] + "⟧"
}

func watchModel(s *stack, text string) {
	if s.hermesModel != nil {
		s.hermesModel.Watch(text)
	} else {
		s.hermesRec.Watch(text)
	}
}

func modelSaw(s *stack) bool {
	if s.hermesModel != nil {
		return s.hermesModel.WatchSeen()
	}
	return s.hermesRec.WatchSeen()
}

// laneH3f runs the arm; called from TestHermesLane after H3-recall.
func laneH3f(t *testing.T, l *laneRun) {
	s := l.s
	var mirrored memChunk
	var token string
	l.step("H3f-add", promptH3fAdd, false, func(hermesRun) error {
		entry, target := hermesEntry(t, s, h3fFact)
		if entry == "" {
			return fmt.Errorf("Hermes's own memory holds no %s entry", h3fFact)
		}
		token = expectedToken(target, entry)
		var rows []memChunk
		waitFor(t, "the mirrored note", slow(time.Minute), func() bool {
			rows = memoryChunks(t, h3fFact)
			return len(rows) > 0
		})
		for _, r := range rows {
			if !mirrorTokenRe.MatchString(r.content) {
				// The model also stored the fact with vornik_remember: a note
				// the mirror did not write and a removal cannot reach.
				t.Fatalf("H3f-add: a %s note without a mirror token is in Vornik (%s): %s", h3fFact, r.id, r.content)
			}
			if strings.Contains(r.content, token) {
				mirrored = r
			}
		}
		if mirrored.id == "" {
			return fmt.Errorf("no chunk carries the token %s computed from Hermes's entry %q; chunks: %v", token, entry, rows)
		}
		return nil
	})
	t.Logf("H3f: captured chunk %s carrying %s", mirrored.id, token)

	// Every attempt starts from the state the step began with (lane design,
	// "Amendment 2026-10-14 — H3f-forget retries start from the step's
	// state", GitHub #380): before attempt 1 the state is recorded; before
	// a retry, the notes naming the fact that the failed attempt left are
	// removed (and logged), and Hermes's files and the mirrored chunk are
	// put back. No reset follows the last attempt, so a re-store there
	// still fails the step.
	var start h3fStart
	resetH3f := func() {
		attempt := l.attempts["H3f-forget"]
		if attempt == 1 {
			start = h3fStart{ids: map[string]bool{}, files: hermesMemoryFiles(s), mirrored: mirrored}
			for _, r := range memoryChunks(t, "") {
				start.ids[r.id] = true
			}
			start.mirrored.content = memoryChunkRaw(t, mirrored.id)
			return
		}
		if err := h3fResetAttempt(start, attempt-1, h3fLaneOps(t, s)); err != nil {
			t.Fatalf("H3f-forget: %v", err)
		}
	}
	l.step("H3f-forget", promptH3fDel, false, func(hermesRun) error {
		// First, and on every attempt: a note naming the fact stored again
		// (GitHub #380: the model called vornik_remember for the forget and a
		// padded copy was admitted). It is the worst outcome, so it is named
		// even when Hermes's own memory also still holds the entry, and a
		// note re-stored by an earlier attempt fails every later one.
		if restored := h3fRestored(memoryChunks(t, ""), mirrored.id); len(restored) > 0 {
			return fmt.Errorf("the fact was stored in Vornik again after the forget (%d recallable note(s) name it besides the mirrored chunk %s): first %s (%q) %q",
				len(restored), mirrored.id, restored[0].id, restored[0].status, tail(restored[0].content, 300))
		}
		if entry, _ := hermesEntry(t, s, h3fFact); entry != "" {
			return fmt.Errorf("Hermes's own memory still holds %q", entry)
		}
		ok := false
		waitFor(t, "the mirrored note to be refuted", slow(time.Minute), func() bool {
			for _, r := range memoryChunks(t, h3fFact) {
				if r.id == mirrored.id && r.status == "refuted" {
					ok = true
				}
			}
			return ok
		})
		if restored := h3fRestored(memoryChunks(t, ""), mirrored.id); len(restored) > 0 {
			return fmt.Errorf("another note naming the fact is still recallable after the forget: %s (%q) %q",
				restored[0].id, restored[0].status, tail(restored[0].content, 300))
		}
		return nil
	}, resetH3f)
	if hits := recallHits(t, s, "physiotherapist "+h3fFact); strings.Contains(hits, h3fFact) {
		t.Fatalf("H3f: the forgotten note is still recalled: %s", tail(hits, 600))
	}

	watchModel(s, h3fFact)
	l.step("H3f-ask", promptH3fAsk, false, func(r hermesRun) error {
		if strings.Contains(r.Reply, h3fFact) {
			return fmt.Errorf("a fresh session still answers %s", h3fFact)
		}
		if modelSaw(s) {
			return fmt.Errorf("the forgotten fact reached the model (prefetch block or a recall result)")
		}
		return nil
	})
}
