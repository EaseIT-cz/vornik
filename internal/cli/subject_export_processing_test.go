package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #470 (GDPR design §4.22, §4.20.1 item 14): the Art 15 report carried
// none of §4.4(b)-(d), the recipients and retention windows among them. Found
// at: audit (the 2026-10-21 status audit read the artefact a subject receives
// from `vornikctl subject export`); this test reads that artefact, built on a
// real SQLite store from a configuration.
func TestSubjectExport_StatesRecipientsAndRetentionFromConfig(t *testing.T) {
	db := sqlitetest.File(t, "export-processing.db")
	repo := sqlite.NewDataSubjectRepository(db.DB)
	ctx := context.Background()

	const subj = "ds_export_470"
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: subj, DisplayName: "Jane Example"}); err != nil {
		t.Fatalf("CreateSubject: %v", err)
	}
	now := time.Now().UTC()
	req := datasubject.Request{ID: "dsr_export_470", SubjectID: subj, Kind: datasubject.RequestAccess,
		State: datasubject.StateOpen, OpenedAt: now}
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if err := req.Verify("operator", "authenticated session", now); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	cfg := &config.Config{}
	cfg.Retention.Enabled = false
	cfg.Retention.ChatAuditDays = 45
	cfg.Chat.Enabled = true
	cfg.Chat.Provider = "router"
	cfg.Chat.Router.Default = "vertex"
	cfg.Chat.Router.Vertex.Enabled = true
	cfg.Chat.Router.Vertex.APIKey = "VERTEX-KEY-MUST-NOT-LEAK"
	cfg.Chat.Router.Vertex.ProjectID = "gcp-project-must-not-leak"
	cfg.Chat.Router.Vertex.Location = "europe-west4"
	cfg.Chat.Router.ClaudeCLI.Enabled = true

	_, _, blob, _, err := assembleSubjectExport(ctx, repo, req, cfg)
	if err != nil {
		t.Fatalf("assembleSubjectExport: %v", err)
	}
	for _, secret := range []string{"VERTEX-KEY-MUST-NOT-LEAK", "gcp-project-must-not-leak"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("the report must carry hosts and key names only, never %q:\n%s", secret, blob)
		}
	}

	var got struct {
		Purposes   []datasubject.ProcessingActivity `json:"purposes"`
		Recipients []datasubject.Recipient          `json:"recipients"`
		Retention  datasubject.RetentionStatement   `json:"retention"`
	}
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("the artefact is JSON: %v", err)
	}
	if len(got.Purposes) == 0 {
		t.Errorf("Art 15(1)(a): the purposes are missing:\n%s", blob)
	}

	// Art 15(1)(c): a configured provider appears as a recipient; one whose
	// destination Vornik cannot see is stated as unknown.
	var vertex, cli *datasubject.Recipient
	for i := range got.Recipients {
		switch got.Recipients[i].ConfigKey {
		case "chat.router.vertex":
			vertex = &got.Recipients[i]
		case "chat.router.claude_cli":
			cli = &got.Recipients[i]
		}
	}
	if vertex == nil {
		t.Fatalf("Art 15(1)(c): the enabled Vertex sub-provider must be a recipient:\n%s", blob)
	}
	if vertex.Location != datasubject.LocationRemote || vertex.Destination != "europe-west4-aiplatform.googleapis.com" ||
		vertex.Region != "europe-west4" {
		t.Errorf("Vertex must read remote, its host and its configured region, got %+v", *vertex)
	}
	if cli == nil || cli.Location != datasubject.Unknown || !strings.HasPrefix(cli.Destination, datasubject.Unknown) {
		t.Errorf("a command-line provider's destination must be stated as unknown, got %+v", cli)
	}

	// Art 15(1)(d): a configured window appears with its config key; the
	// sweeper being off is stated, not hidden behind the number.
	if !got.Retention.Known || got.Retention.SweeperEnabled {
		t.Errorf("retention must be known and the sweeper off, got %+v", got.Retention)
	}
	var chatAudit *datasubject.RetentionWindow
	for i := range got.Retention.Windows {
		if got.Retention.Windows[i].ConfigKey == "retention.chat_audit_days" {
			chatAudit = &got.Retention.Windows[i]
		}
	}
	if chatAudit == nil {
		t.Fatalf("the configured chat audit window must appear with its config key:\n%s", blob)
	}
	if chatAudit.Days != 45 || chatAudit.Source != datasubject.WindowConfigured {
		t.Errorf("chat audit must read 45 days, configured, got %+v", *chatAudit)
	}
	if !strings.Contains(chatAudit.Period, "not applied") {
		t.Errorf("with retention.enabled false the period must say the window is not applied, got %q", chatAudit.Period)
	}
	var memChunks *datasubject.RetentionWindow
	for i := range got.Retention.Windows {
		if got.Retention.Windows[i].ConfigKey == "retention.memory_chunks_days" {
			memChunks = &got.Retention.Windows[i]
		}
	}
	if memChunks == nil || memChunks.Source != datasubject.WindowNotSet || memChunks.Days != 0 {
		t.Errorf("an opt-in window left unset must read not_set, got %+v", memChunks)
	}
}

// Review 6a1e: an endpoint with no host is never called local or remote.
func TestModelRecipient_NoHostIsUnknown(t *testing.T) {
	for _, ep := range []string{"", "not a url", "/v1"} {
		r := modelRecipient("http", "chat.endpoint", ep, false, true, "")
		if r.Location != datasubject.Unknown || !strings.HasPrefix(r.Destination, datasubject.Unknown+": ") ||
			len(r.Destination) <= len(datasubject.Unknown)+2 {
			t.Errorf("endpoint %q must read unknown with a reason, got %+v", ep, r)
		}
	}
}

// Review 6a1e: a configuration that cannot be read must not block the right
// of access; the export proceeds with the unknowns stated, and says why.
func TestLoadExportConfig_FailureIsReportedNotFatal(t *testing.T) {
	var log strings.Builder
	cfg := loadExportConfig(func() (*config.Config, string, error) {
		return nil, "", errors.New("yaml: line 3: bad indentation")
	}, &log)
	if cfg != nil {
		t.Fatal("a failed load must yield nil, which the report states as unknown")
	}
	if !strings.Contains(log.String(), "bad indentation") || !strings.Contains(log.String(), "unknown") {
		t.Errorf("the failure must be logged with its cause, got %q", log.String())
	}
	want := &config.Config{}
	if got := loadExportConfig(func() (*config.Config, string, error) { return want, "", nil }, &log); got != want {
		t.Error("a successful load must pass the config through")
	}
}

// Review 6a1e: the tool-audit floor's warning is stated on the window, not
// dropped. A floor below six months without an override is ignored; the
// report must say so beside the 180 days it applies.
func TestRetentionStatement_StatesTheToolAuditFloorWarning(t *testing.T) {
	cfg := &config.Config{}
	cfg.Retention.Enabled = true
	cfg.Retention.ToolAuditFloorDays = 30
	for _, w := range subjectRetention(cfg).Windows {
		if w.ConfigKey == "retention.tool_audit_days" {
			if w.Days != 180 || !strings.Contains(w.Period, "tool_audit_floor_days=30 ignored") {
				t.Errorf("the ignored floor must be stated beside the 180 days applied, got %+v", w)
			}
			return
		}
	}
	t.Fatal("the tool audit window is missing")
}

// With the sweeper on, a window left at zero reads its compiled default.
func TestRetentionStatement_DefaultsWhenSweeperOn(t *testing.T) {
	cfg := &config.Config{}
	cfg.Retention.Enabled = true
	st := subjectRetention(cfg)
	if !st.SweeperEnabled {
		t.Fatal("sweeper_enabled must follow retention.enabled")
	}
	for _, w := range st.Windows {
		if w.ConfigKey == "retention.tool_audit_days" {
			if w.Days != 180 || w.Source != datasubject.WindowDefault || strings.Contains(w.Period, "not applied") {
				t.Errorf("tool audit must read the 180-day default, applied, got %+v", w)
			}
			return
		}
	}
	t.Fatal("the tool audit window is missing")
}
