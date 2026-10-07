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
	h3fFact      = "Svoboda"
	promptH3fAdd = "Use your own memory tool, not Vornik, to save this to the user profile: my physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic."
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

type memChunk struct{ id, status, content string }

// memoryChunks reads the memory project's chunks mentioning text, straight
// from the lane's Postgres (refuted rows included: recall hides them).
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

	l.step("H3f-forget", promptH3fDel, false, func(hermesRun) error {
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
		for _, r := range memoryChunks(t, h3fFact) {
			if r.id != mirrored.id && r.status != "refuted" {
				return fmt.Errorf("another %s chunk is still recallable: %s (%s)", h3fFact, r.id, r.status)
			}
		}
		return nil
	})
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
