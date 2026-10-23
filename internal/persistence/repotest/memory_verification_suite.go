package repotest

// Memory verification: the cross-driver contract for provenance records and
// the audited, recoverable changes of chunk verification state (design 22,
// "Amendment 2026-10-21 (#482): a companion-delegated chunk is verified only
// by provenance").
//
// GitHub #482. The companion-chain predicate decides whether a chunk's role
// may verify it; the change and restore decide whether ~27,000 production
// chunks can be de-verified and put back. Both are SQL, so "it works" is a
// claim about a driver: both lanes run this suite (internal/persistence/sqlite
// in go test ./..., internal/persistence/postgres under -tags=integration).

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// VerificationSeedChunk is one chunk the suite plants.
type VerificationSeedChunk struct {
	ID, ProjectID, ArtifactID, TaskID string
	ProducerRole, ValidatorRole       string
	Status, SourceName, RepoScope     string
	Content                           string
}

// MemoryVerificationHarness is what a driver supplies.
type MemoryVerificationHarness struct {
	// Repo is set by RunMemoryVerificationSuite from its repo argument.
	Repo  persistence.MemoryVerificationRepository
	Tasks persistence.TaskRepository
	// SeedChunk plants a chunk (and its artifact row where the driver needs
	// one).
	SeedChunk func(ctx context.Context, c VerificationSeedChunk) error
	// ReadChunk returns a chunk's status and validator role ("" for NULL).
	ReadChunk func(ctx context.Context, id string) (status, validator string, err error)
	// SetChunk overwrites a chunk's status and validator role.
	SetChunk func(ctx context.Context, id, status, validator string) error
	// SetParent points a task at a parent ("" clears it), bypassing the
	// repository, to build cycles and orphans.
	SetParent func(ctx context.Context, taskID, parentID string) error
	// CountBackup counts an event's backup rows.
	CountBackup func(ctx context.Context, eventID string) (int, error)
	// SeedKey plants an api_keys row: a companion key has a client kind,
	// an agent-admin key agentAdmin or a namespace.
	SeedKey func(ctx context.Context, id, project, clientKind string, agentAdmin bool, namespace string) error
}

func mvTask(t *testing.T, h MemoryVerificationHarness, project string, src persistence.TaskCreationSource, parent string) string {
	t.Helper()
	id := uniqueID("task")
	task := &persistence.Task{ID: id, ProjectID: project, CreationSource: src, Status: persistence.TaskStatusCompleted,
		Priority: 50, MaxAttempts: 1, Payload: []byte(`{}`)}
	if parent != "" {
		p := parent
		task.ParentTaskID = &p
	}
	if err := h.Tasks.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return id
}

func mvKeyedTask(t *testing.T, h MemoryVerificationHarness, project string, src persistence.TaskCreationSource, keyID string, actorID ...string) string {
	t.Helper()
	id := uniqueID("task")
	task := &persistence.Task{ID: id, ProjectID: project, CreationSource: src, Status: persistence.TaskStatusCompleted,
		Priority: 50, MaxAttempts: 1, Payload: []byte(`{}`)}
	if len(actorID) > 0 && actorID[0] != "" {
		a := actorID[0]
		task.CreatedByActor = &a
	}
	if keyID != "" {
		k := keyID
		task.CreatedByAPIKeyID = &k
	}
	if err := h.Tasks.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return id
}

// mvCasePrincipalAndUnlistedSources: GitHub #482, coordinator security
// finding 1 (2026-10-22). The predicate keyed on one source value: a broker
// schedule fire creates its task SCHEDULED with the companion's key, and an
// A2A task is A2A, and neither counted as delegated, so a companion or
// agent-admin key still minted verified chunks through them. Now the
// principal decides (the creating key's class), and only USER and
// AUTONOMOUS count as trusted sources; anything else is delegated.
func mvCasePrincipalAndUnlistedSources(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	companionKey, adminKey, nsKey, plainKey := uniqueID("key"), uniqueID("key"), uniqueID("key"), uniqueID("key")
	for _, k := range []struct {
		id, kind string
		admin    bool
		ns       string
	}{{companionKey, "claude-code", false, ""}, {adminKey, "", true, "claudecode"}, {nsKey, "", false, "hermes"}, {plainKey, "", false, ""}} {
		if err := h.SeedKey(ctx, k.id, project, k.kind, k.admin, k.ns); err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}
	brokerFire := mvKeyedTask(t, h, project, persistence.TaskCreationSourceScheduled, companionKey)
	brokerChild := mvTask(t, h, project, persistence.TaskCreationSourceDelegation, brokerFire)
	a2a := mvKeyedTask(t, h, project, persistence.TaskCreationSourceA2A, "")
	scheduled := mvKeyedTask(t, h, project, persistence.TaskCreationSourceScheduled, "")
	userByCompanion := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, companionKey)
	userByAdmin := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, adminKey)
	userByNamespace := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, nsKey)
	userByGoneKey := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, uniqueID("key-gone"))
	userByPlainKey := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, plainKey)
	operator := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "user:op")
	autonomous := mvKeyedTask(t, h, project, persistence.TaskCreationSourceAutonomous, "", "system:autonomy")
	operatorChild := mvTask(t, h, project, persistence.TaskCreationSourceDelegation, operator)
	ids := []string{brokerFire, brokerChild, a2a, scheduled, userByCompanion, userByAdmin, userByNamespace,
		userByGoneKey, userByPlainKey, operator, autonomous, operatorChild}
	got, err := h.Repo.CompanionDelegatedTasks(ctx, ids)
	if err != nil {
		t.Fatalf("CompanionDelegatedTasks: %v", err)
	}
	for id, want := range map[string]bool{
		brokerFire: true, brokerChild: true, // a broker schedule fire carries the companion key
		a2a:             true,                                           // an unlisted source
		scheduled:       true,                                           // an unlisted source, whoever scheduled it (fail closed)
		userByCompanion: true, userByAdmin: true, userByNamespace: true, // the principal decides
		userByGoneKey:  true, // a key that cannot be read
		userByPlainKey: false, operator: false, autonomous: false, operatorChild: false,
	} {
		if got[id] != want {
			t.Errorf("task %q: delegated=%v, want %v", id, got[id], want)
		}
	}
}

// mvCaseUnattributedRootsAreDelegated: GitHub #482, coordinator security
// finding 3 (2026-10-22). A USER or AUTONOMOUS root with no key id counted as
// trusted, so a task created by a static key (the broker's, the A2A
// adapter's) or by any path that stamps no principal came out as the
// operator's. Now only an attributable operator principal is trusted: a
// session user (`user:<id>`), a plain DB-backed project key, or the
// autonomy loop's own `system:autonomy`.
func mvCaseUnattributedRootsAreDelegated(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	plainKey := uniqueID("key")
	if err := h.SeedKey(ctx, plainKey, project, "", false, ""); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	unattributed := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "")
	staticKey := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "system:static_key:0123456789abcdef")
	webhook := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "system:webhook:github")
	bareAutonomy := mvKeyedTask(t, h, project, persistence.TaskCreationSourceAutonomous, "")
	spoofedAutonomy := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "system:autonomy")
	session := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "user:operator")
	plain := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, plainKey, "api_key:"+plainKey)
	autonomy := mvKeyedTask(t, h, project, persistence.TaskCreationSourceAutonomous, "", "system:autonomy")
	sessionChild := mvTask(t, h, project, persistence.TaskCreationSourceDelegation, session)
	unattributedChild := mvTask(t, h, project, persistence.TaskCreationSourceDelegation, unattributed)
	ids := []string{unattributed, staticKey, webhook, bareAutonomy, spoofedAutonomy, session, plain, autonomy, sessionChild, unattributedChild}
	got, err := h.Repo.CompanionDelegatedTasks(ctx, ids)
	if err != nil {
		t.Fatalf("CompanionDelegatedTasks: %v", err)
	}
	for id, want := range map[string]bool{
		unattributed: true, staticKey: true, webhook: true, bareAutonomy: true,
		spoofedAutonomy:   true, // the autonomy principal counts only on an AUTONOMOUS task
		unattributedChild: true,
		session:           false, plain: false, autonomy: false, sessionChild: false,
	} {
		if got[id] != want {
			t.Errorf("task %q: delegated=%v, want %v", id, got[id], want)
		}
	}
}

// VerificationKeyName is the api_keys.name a harness gives a seeded key: the
// id itself for an agent key (id "agent:..."), so the suite can plant the
// executor's per-task and warm-pool key shapes; "k-<id>" otherwise.
func VerificationKeyName(id string) string {
	if strings.HasPrefix(id, "agent:") {
		return id
	}
	return "k-" + id
}

// mvCaseOperatorPrincipals: GitHub #482 round-3 review A3 and #499 (operator
// decision A19). An agent per-task or warm-pool key looks plain (no client
// kind, not admin, no namespace) and must not count as the operator's; the
// operator's listed static key (`system:operator_key:<hash>`) does, on a USER
// root, and on no other source.
func mvCaseOperatorPrincipals(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	taskKey, warmKey := "agent:task_"+uniqueID("t"), "agent:warm_"+project+":coder"
	for _, k := range []string{taskKey, warmKey} {
		if err := h.SeedKey(ctx, k, project, "", false, ""); err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}
	byTaskKey := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, taskKey, "api_key:"+taskKey)
	byWarmKey := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, warmKey, "api_key:"+warmKey)
	operatorKey := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "system:operator_key:0123456789abcdef")
	operatorKeyScheduled := mvKeyedTask(t, h, project, persistence.TaskCreationSourceScheduled, "", "system:operator_key:0123456789abcdef")
	operatorKeyShort := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "system:operator_key:")
	ids := []string{byTaskKey, byWarmKey, operatorKey, operatorKeyScheduled, operatorKeyShort}
	got, err := h.Repo.CompanionDelegatedTasks(ctx, ids)
	if err != nil {
		t.Fatalf("CompanionDelegatedTasks: %v", err)
	}
	for id, want := range map[string]bool{
		byTaskKey: true, byWarmKey: true, // an agent's key is not the operator's
		operatorKey:          false,
		operatorKeyScheduled: true, // only a USER root
		operatorKeyShort:     true, // a principal with no key id
	} {
		if got[id] != want {
			t.Errorf("task %q: delegated=%v, want %v", id, got[id], want)
		}
	}
}

func mvChunk(t *testing.T, h MemoryVerificationHarness, c VerificationSeedChunk) {
	t.Helper()
	if c.Content == "" {
		c.Content = "content of " + c.ID
	}
	if c.SourceName == "" {
		c.SourceName = "seed.md" // NOT NULL on Postgres
	}
	if err := h.SeedChunk(context.Background(), c); err != nil {
		t.Fatalf("seed chunk %s: %v", c.ID, err)
	}
}

func mvState(t *testing.T, h MemoryVerificationHarness, id, wantStatus, wantValidator string) {
	t.Helper()
	st, v, err := h.ReadChunk(context.Background(), id)
	if err != nil {
		t.Fatalf("read chunk %s: %v", id, err)
	}
	if st != wantStatus || v != wantValidator {
		t.Fatalf("chunk %s: status=%q validator=%q, want %q %q", id, st, v, wantStatus, wantValidator)
	}
}

// RunMemoryVerificationSuite runs the contract.
func RunMemoryVerificationSuite(t *testing.T, repo persistence.MemoryVerificationRepository, h MemoryVerificationHarness) {
	h.Repo = repo
	t.Helper()
	t.Run("CompanionChain_decides_and_fails_closed", func(t *testing.T) { mvCaseCompanionChainDecidesAndFailsClosed(t, h) })
	t.Run("Provenance_round_trip_and_evidence_check", func(t *testing.T) { mvCaseProvenanceRoundTripAndEvidenceCheck(t, h) })
	t.Run("Candidates_are_role_of_record_stamps_only", func(t *testing.T) { mvCaseCandidatesAreRoleOfRecordStampsOnly(t, h) })
	t.Run("Apply_backs_up_changes_and_counts_in_one_event", func(t *testing.T) { mvCaseApplyBacksUpChangesAndCountsInOneEvent(t, h) })
	t.Run("Restore_puts_back_only_unchanged_rows", func(t *testing.T) { mvCaseRestorePutsBackOnlyUnchangedRows(t, h) })
	t.Run("Principal_and_unlisted_sources_are_delegated", func(t *testing.T) { mvCasePrincipalAndUnlistedSources(t, h) })
	t.Run("Unattributed_roots_are_delegated", func(t *testing.T) { mvCaseUnattributedRootsAreDelegated(t, h) })
	t.Run("Operator_principals", func(t *testing.T) { mvCaseOperatorPrincipals(t, h) })
	t.Run("Heal_stamps_only_never_stamped_chunks", func(t *testing.T) { mvCaseHealStampsOnlyNeverStampedChunks(t, h) })
}

// GitHub #506 (security review 2026-10-23): the sweep's heal stamps
// provenance:unavailable only on chunks that were never stamped. A chunk a
// verification event de-verified (it has a backup row) or a person refuted
// is left as it is, so a restart never re-verifies what someone demoted.
func mvCaseHealStampsOnlyNeverStampedChunks(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	fresh, demoted, refuted := "heal-fresh-"+project, "heal-demoted-"+project, "heal-refuted-"+project
	cf, cd, cr := uniqueID("c"), uniqueID("c"), uniqueID("c")
	mvChunk(t, h, VerificationSeedChunk{ID: cf, ProjectID: project, ArtifactID: fresh, ProducerRole: "rag-ingester", Status: "unverified"})
	mvChunk(t, h, VerificationSeedChunk{ID: cd, ProjectID: project, ArtifactID: demoted, ProducerRole: "rag-ingester", ValidatorRole: "rag-ingester", Status: "verified"})
	mvChunk(t, h, VerificationSeedChunk{ID: cr, ProjectID: project, ArtifactID: refuted, ProducerRole: "rag-ingester", Status: "refuted"})
	backfill := &persistence.MemoryVerificationEvent{ProjectID: project, Kind: "backfill", Actor: "operator"}
	if err := h.Repo.ApplyVerificationChange(ctx, backfill, []persistence.ArtifactVerificationChange{
		{ArtifactID: demoted, NewStatus: "unverified"},
	}); err != nil {
		t.Fatal(err)
	}
	mvState(t, h, cd, "unverified", "")

	heal := &persistence.MemoryVerificationEvent{ProjectID: project, Kind: "sweep", Actor: "provenance-sweep"}
	var changes []persistence.ArtifactVerificationChange
	for _, a := range []string{fresh, demoted, refuted} {
		changes = append(changes, persistence.ArtifactVerificationChange{ArtifactID: a, NewStatus: "verified",
			NewValidatorRole: "provenance:unavailable", OnlyNeverStamped: true})
	}
	if err := h.Repo.ApplyVerificationChange(ctx, heal, changes); err != nil {
		t.Fatal(err)
	}
	mvState(t, h, cf, "verified", "provenance:unavailable")
	mvState(t, h, cd, "unverified", "") // de-verified by an event: never re-stamped
	mvState(t, h, cr, "refuted", "")
}

func mvCaseCompanionChainDecidesAndFailsClosed(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	companion := mvTask(t, h, project, persistence.TaskCreationSourceCompanion, "")
	child := mvTask(t, h, project, persistence.TaskCreationSourceDelegation, companion)
	grandchild := mvTask(t, h, project, persistence.TaskCreationSourceRoute, child)
	user := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "user:op")
	userChild := mvTask(t, h, project, persistence.TaskCreationSourceDelegation, user)
	autonomous := mvKeyedTask(t, h, project, persistence.TaskCreationSourceAutonomous, "", "system:autonomy")
	orphan := mvTask(t, h, project, persistence.TaskCreationSourceDelegation, "")
	cycA := mvTask(t, h, project, persistence.TaskCreationSourceUser, "")
	cycB := mvTask(t, h, project, persistence.TaskCreationSourceDelegation, cycA)
	if err := h.SetParent(ctx, cycA, cycB); err != nil {
		t.Fatalf("make cycle: %v", err)
	}
	missing := uniqueID("task-missing")
	got, err := h.Repo.CompanionDelegatedTasks(ctx, []string{companion, child, grandchild, user, userChild,
		autonomous, orphan, cycA, missing, "", companion})
	if err != nil {
		t.Fatalf("CompanionDelegatedTasks: %v", err)
	}
	for id, want := range map[string]bool{
		companion: true, child: true, grandchild: true, // the chain launders nothing
		user: false, userChild: false, autonomous: false, // operator and unattended work
		orphan:  true, // a child whose parent is gone: origin unknown
		cycA:    true, // a cycle never reaches a root
		missing: true, // no such task
		"":      true, // no task at all
	} {
		if got[id] != want {
			t.Errorf("task %q: delegated=%v, want %v", id, got[id], want)
		}
	}

}

func mvCaseProvenanceRoundTripAndEvidenceCheck(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	art := uniqueID("art")
	checked := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	p := &persistence.ChunkProvenance{ProjectID: project, ArtifactID: art, RepoScope: "github.com/a/b",
		DocumentPath: "docs" + "/low-level-design/x.md", CommitSHA: "c1", ContentBlobSHA: "b1", OriginBlobSHA: "b1",
		State: "verified", CheckedAt: checked, VerifiedAt: &checked, CreatedAt: checked}
	if err := h.Repo.UpsertProvenance(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	later := checked.Add(time.Hour)
	p2 := *p
	p2.State, p2.Reason, p2.OriginBlobSHA, p2.CheckedAt, p2.CreatedAt = "stale", "blob_changed", "b2", later, later
	if err := h.Repo.UpsertProvenance(ctx, &p2); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}
	got, err := h.Repo.GetProvenance(ctx, project, art)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.State != "stale" || got.Reason != "blob_changed" || !got.CheckedAt.Equal(later) || !got.CreatedAt.Equal(checked) ||
		got.VerifiedAt == nil || !got.VerifiedAt.Equal(checked) || got.OriginBlobSHA != "b2" {
		t.Fatalf("round trip: %+v", got)
	}
	// A verified record without its evidence is refused by the database.
	bad := &persistence.ChunkProvenance{ProjectID: project, ArtifactID: uniqueID("art"), State: "verified",
		ContentBlobSHA: "b1", OriginBlobSHA: "b2", CommitSHA: "c", CheckedAt: checked, VerifiedAt: &checked}
	if err := h.Repo.UpsertProvenance(ctx, bad); err == nil {
		t.Fatal("a verified record whose blobs differ was stored")
	}
	if none, err := h.Repo.GetProvenance(ctx, project, uniqueID("art")); err != nil || none != nil {
		t.Fatalf("absent record: %v %v", none, err)
	}
	list, err := h.Repo.ListProvenance(ctx, project, "", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	projects, err := h.Repo.ListProvenanceProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range projects {
		found = found || p == project
	}
	if !found {
		t.Fatalf("project %s not listed in %v", project, projects)
	}

}

func mvCaseCandidatesAreRoleOfRecordStampsOnly(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	task := mvTask(t, h, project, persistence.TaskCreationSourceCompanion, "")
	a1, a2, a3, a4, a5 := "a1-"+project, "a2-"+project, "a3-"+project, "a4-"+project, "a5-"+project
	mvChunk(t, h, VerificationSeedChunk{ID: uniqueID("c"), ProjectID: project, ArtifactID: a1, TaskID: task, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified", SourceName: "review.md"})
	mvChunk(t, h, VerificationSeedChunk{ID: uniqueID("c"), ProjectID: project, ArtifactID: a1, TaskID: task, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified", SourceName: "review.md"})
	mvChunk(t, h, VerificationSeedChunk{ID: uniqueID("c"), ProjectID: project, ArtifactID: a2, ProducerRole: "companion:claude-code", ValidatorRole: "companion:claude-code", Status: "verified"})
	mvChunk(t, h, VerificationSeedChunk{ID: uniqueID("c"), ProjectID: project, ArtifactID: a3, ProducerRole: "rag-ingester", ValidatorRole: "provenance:origin/main", Status: "verified"})
	mvChunk(t, h, VerificationSeedChunk{ID: uniqueID("c"), ProjectID: project, ArtifactID: a4, ProducerRole: "operator_correction", ValidatorRole: "operator_correction", Status: "verified"})
	mvChunk(t, h, VerificationSeedChunk{ID: uniqueID("c"), ProjectID: project, ArtifactID: a5, ProducerRole: "reviewer", Status: "unverified"})
	got, err := h.Repo.ListRoleOfRecordCandidates(ctx, project, "", 10)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(got) != 1 || got[0].ArtifactID != a1 || got[0].Chunks != 2 || got[0].TaskID != task ||
		got[0].ProducerRole != "reviewer" || got[0].SourceName != "review.md" {
		t.Fatalf("candidates = %+v, want only %s with 2 chunks", got, a1)
	}
	if after, err := h.Repo.ListRoleOfRecordCandidates(ctx, project, a1, 10); err != nil || len(after) != 0 {
		t.Fatalf("paging after the last: %+v %v", after, err)
	}

	// Design review 565b finding 2: an artifact co-written by an operator's
	// task and a companion's is one candidate with NO task id, and an empty
	// id answers delegated, so the backfill evaluates it by provenance
	// rather than dropping it.
	opTask := mvKeyedTask(t, h, project, persistence.TaskCreationSourceUser, "", "user:operator")
	a6 := "a6-" + project
	mvChunk(t, h, VerificationSeedChunk{ID: uniqueID("c"), ProjectID: project, ArtifactID: a6, TaskID: opTask, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified"})
	mvChunk(t, h, VerificationSeedChunk{ID: uniqueID("c"), ProjectID: project, ArtifactID: a6, TaskID: task, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified"})
	co, err := h.Repo.ListRoleOfRecordCandidates(ctx, project, a1, 10) // the page after a1 (the cursor)
	if err != nil || len(co) != 1 || co[0].ArtifactID != a6 || co[0].TaskID != "" || co[0].Chunks != 2 {
		t.Fatalf("co-written candidate = %+v %v, want %s with no task id and 2 chunks", co, err, a6)
	}
	d, err := h.Repo.CompanionDelegatedTasks(ctx, []string{co[0].TaskID, opTask})
	if err != nil || !d[""] || d[opTask] {
		t.Fatalf("delegated = %v %v, want the co-written candidate delegated and the operator task alone not", d, err)
	}
}

func mvCaseApplyBacksUpChangesAndCountsInOneEvent(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	art, art2 := "x1-"+project, "x2-"+project
	c1, c2, c3, c4 := uniqueID("c"), uniqueID("c"), uniqueID("c"), uniqueID("c")
	mvChunk(t, h, VerificationSeedChunk{ID: c1, ProjectID: project, ArtifactID: art, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified"})
	mvChunk(t, h, VerificationSeedChunk{ID: c2, ProjectID: project, ArtifactID: art, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified"})
	mvChunk(t, h, VerificationSeedChunk{ID: c3, ProjectID: project, ArtifactID: art, ProducerRole: "reviewer", Status: "superseded"})
	mvChunk(t, h, VerificationSeedChunk{ID: c4, ProjectID: project, ArtifactID: art2, ProducerRole: "rag-ingester", ValidatorRole: "rag-ingester", Status: "verified"})
	now := time.Now().UTC()
	ev := &persistence.MemoryVerificationEvent{ProjectID: project, Kind: persistence.MemoryVerificationKindBackfill,
		Actor: "operator", Reason: "suite", Examined: 2}
	err := h.Repo.ApplyVerificationChange(ctx, ev, []persistence.ArtifactVerificationChange{
		{ArtifactID: art, NewStatus: "unverified"},
		{ArtifactID: art2, NewStatus: "verified", NewValidatorRole: "provenance:origin/main",
			Provenance: &persistence.ChunkProvenance{RepoScope: "s", DocumentPath: "CLAUDE.md", CommitSHA: "c",
				ContentBlobSHA: "b", OriginBlobSHA: "b", State: "verified", CheckedAt: now, VerifiedAt: &now}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ev.ID == "" || ev.Deverified != 2 || ev.Verified != 1 {
		t.Fatalf("event counts: %+v", ev)
	}
	mvState(t, h, c1, "unverified", "")
	mvState(t, h, c2, "unverified", "")
	mvState(t, h, c3, "superseded", "") // a terminal row is never touched
	mvState(t, h, c4, "verified", "provenance:origin/main")
	if n, err := h.CountBackup(ctx, ev.ID); err != nil || n != 3 {
		t.Fatalf("backup rows = %d (%v), want 3", n, err)
	}
	if p, err := h.Repo.GetProvenance(ctx, project, art2); err != nil || p == nil || p.State != "verified" {
		t.Fatalf("provenance written with the change: %+v %v", p, err)
	}
	stored, err := h.Repo.GetVerificationEvent(ctx, ev.ID)
	if err != nil || stored == nil || stored.Deverified != 2 || stored.Verified != 1 || stored.Examined != 2 ||
		stored.Actor != "operator" || stored.Reason != "suite" || stored.Kind != "backfill" {
		t.Fatalf("stored event: %+v %v", stored, err)
	}

	// One artifact twice in one event: refused, and nothing is written.
	c5 := uniqueID("c")
	mvChunk(t, h, VerificationSeedChunk{ID: c5, ProjectID: project, ArtifactID: "x3-" + project, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified"})
	dup := &persistence.MemoryVerificationEvent{ProjectID: project, Kind: "backfill", Actor: "operator"}
	if err := h.Repo.ApplyVerificationChange(ctx, dup, []persistence.ArtifactVerificationChange{
		{ArtifactID: "x3-" + project, NewStatus: "unverified"}, {ArtifactID: "x3-" + project, NewStatus: "unverified"},
	}); err == nil {
		t.Fatal("a duplicate artifact was accepted")
	}
	mvState(t, h, c5, "verified", "reviewer")
	if got, _ := h.Repo.GetVerificationEvent(ctx, dup.ID); got != nil {
		t.Fatal("a refused change left its event behind")
	}

	// OnlyValidatorRole narrows a de-verify to the stamps it names.
	c6, c7 := uniqueID("c"), uniqueID("c")
	mvChunk(t, h, VerificationSeedChunk{ID: c6, ProjectID: project, ArtifactID: "x4-" + project, ProducerRole: "rag-ingester", ValidatorRole: "provenance:origin/main", Status: "verified"})
	mvChunk(t, h, VerificationSeedChunk{ID: c7, ProjectID: project, ArtifactID: "x4-" + project, ProducerRole: "rag-ingester", ValidatorRole: "rag-ingester", Status: "verified"})
	sweep := &persistence.MemoryVerificationEvent{ProjectID: project, Kind: "sweep", Actor: "provenance-sweep"}
	if err := h.Repo.ApplyVerificationChange(ctx, sweep, []persistence.ArtifactVerificationChange{
		{ArtifactID: "x4-" + project, NewStatus: "unverified", OnlyValidatorRole: "provenance:origin/main"},
	}); err != nil {
		t.Fatal(err)
	}
	mvState(t, h, c6, "unverified", "")
	mvState(t, h, c7, "verified", "rag-ingester")

}

func mvCaseRestorePutsBackOnlyUnchangedRows(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	art := "r1-" + project
	c1, c2, c3 := uniqueID("c"), uniqueID("c"), uniqueID("c")
	for _, id := range []string{c1, c2, c3} {
		mvChunk(t, h, VerificationSeedChunk{ID: id, ProjectID: project, ArtifactID: art, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified"})
	}
	ev := &persistence.MemoryVerificationEvent{ProjectID: project, Kind: "backfill", Actor: "operator", Reason: "de-verify"}
	if err := h.Repo.ApplyVerificationChange(ctx, ev, []persistence.ArtifactVerificationChange{{ArtifactID: art, NewStatus: "unverified"}}); err != nil {
		t.Fatal(err)
	}
	// One chunk changes after the event: the restore must leave it.
	if err := h.SetChunk(ctx, c3, "refuted", ""); err != nil {
		t.Fatal(err)
	}
	n, err := h.Repo.CountRestorableSelfCertified(ctx, ev.ID)
	if err != nil || n != 2 {
		t.Fatalf("restorable self-certified = %d (%v), want 2", n, err)
	}
	if _, err := h.Repo.RestoreVerificationEvent(ctx, ev.ID, "operator", ""); err == nil {
		t.Fatal("a restore without a reason was accepted")
	}
	rs, err := h.Repo.RestoreVerificationEvent(ctx, ev.ID, "operator", "the rule was wrong")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rs.Kind != "restore" || rs.RestoresEventID != ev.ID || rs.Examined != 3 || rs.Verified != 2 || rs.Skipped != 1 {
		t.Fatalf("restore event: %+v", rs)
	}
	mvState(t, h, c1, "verified", "reviewer")
	mvState(t, h, c2, "verified", "reviewer")
	mvState(t, h, c3, "refuted", "")
	if n, err := h.CountBackup(ctx, rs.ID); err != nil || n != 2 {
		t.Fatalf("the restore's own backup = %d (%v), want 2", n, err)
	}
	if _, err := h.Repo.RestoreVerificationEvent(ctx, uniqueID("nope"), "operator", "x"); err == nil {
		t.Fatal("restoring an unknown event succeeded")
	}
}
