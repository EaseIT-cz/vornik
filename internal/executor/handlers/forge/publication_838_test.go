package forge

// GitHub #838 (github-app-channel design, amendment 2026-11-19), a port of
// community PR EaseIT-cz/vornik#21: what a forge review publishes is
// scanned, and a review covers the whole change.
//
// Found at: production (TST1 synthetic run against a public test repository:
// the reviewer repeated, and decoded, a fabricated Basic Auth value in a
// public review). Regression tests at: unit, the real post_review and
// fetch_diff handlers with the daemon's detector and a fake forge; the
// provider's size cap at integration (forge/github, an HTTP test server).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

func detector838(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func post838(t *testing.T, prev string, job forgeapi.ForgeJob, d secrets.Detector) *fakeProvider {
	t.Helper()
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	if d != nil {
		h = h.WithSecretsDetector(d)
	}
	in := executor.SystemStepInput{Task: taskWithJob(job), Step: &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(prev)}
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return prov
}

// The secret-shaped spans are masked and the rest of the review is posted.
func TestPublication838_ProseSecretsAreRedacted(t *testing.T) {
	job := forgeapi.ForgeJob{Repo: "o/r", Number: 1}
	for name, tc := range map[string]struct{ prev, leak, kept string }{
		"basic auth header":     {`{"body":"The fixture sends Authorization: Basic ZmFrZTpmYWtl to the API."}`, "ZmFrZTpmYWtl", "The fixture sends"},
		"detached basic value":  {`{"body":"The encoded value was ZmFrZTpmYWtl in config.yaml."}`, "ZmFrZTpmYWtl", "in config.yaml"},
		"decoded basic value":   {`{"review":{"feedback":"It decoded as example_user:fake-passphrase there."}}`, "example_user:fake-passphrase", "there"},
		"credential assignment": {`{"review":{"summary":"The test sets password: fake-demo-value in setup."}}`, "fake-demo-value", "in setup"},
	} {
		prov := post838(t, tc.prev, job, detector838(t))
		if strings.Contains(prov.gotReview.Body, tc.leak) {
			t.Errorf("%s: the value was published: %q", name, prov.gotReview.Body)
		}
		if !strings.Contains(prov.gotReview.Body, "[REDACTED:") || !strings.Contains(prov.gotReview.Body, tc.kept) {
			t.Errorf("%s: want the span masked and the rest posted, got %q", name, prov.gotReview.Body)
		}
	}
}

// The request quote is published text too.
func TestPublication838_TheRequestQuoteIsScanned(t *testing.T) {
	job := forgeapi.ForgeJob{Repo: "o/r", Number: 1, OnDemand: true, CommentAuthor: "maintainer",
		CommentBody: "Please include Authorization: Basic ZmFrZTpmYWtl in the answer"}
	prov := post838(t, `{"body":"Review looks good."}`, job, detector838(t))
	if strings.Contains(prov.gotReview.Body, "ZmFrZTpmYWtl") {
		t.Fatalf("the quoted request leaked the value: %q", prov.gotReview.Body)
	}
}

// Ordinary review text is posted unchanged (the PR's generic rule is not
// taken; measured against the default detector, design amendment).
func TestPublication838_OrdinaryTextIsNotMasked(t *testing.T) {
	body := "KMS ARN arn:aws:kms:eu-central-1:123456789012:key/123e4567-e89b-12d3-a456-426614174000; " +
		"commit 0123456789abcdef0123456789abcdef01234567; token = os.environ[\\\"GH_TOKEN\\\"]; password: ********; secret = getSecret()"
	prov := post838(t, `{"body":"`+body+`"}`, forgeapi.ForgeJob{Repo: "o/r", Number: 1}, detector838(t))
	if strings.Contains(prov.gotReview.Body, "[REDACTED:") {
		t.Fatalf("ordinary review text was masked: %q", prov.gotReview.Body)
	}
}

func TestPublication838_NoDetectorWithholdsTheProse(t *testing.T) {
	prov := post838(t, `{"body":"Looks good, merge it."}`, forgeapi.ForgeJob{Repo: "o/r", Number: 1}, nil)
	if strings.Contains(prov.gotReview.Body, "Looks good") || !strings.Contains(prov.gotReview.Body, "could not be scanned") {
		t.Fatalf("with no detector the prose must be withheld: %q", prov.gotReview.Body)
	}
}

func TestPublication838_TheWithheldApprovalCommentIsRedacted(t *testing.T) {
	prov := &fakeProvider{}
	h := gateHandler(prov, &gateOutcomes{outs: redRun()}, true).WithSecretsDetector(detector838(t))
	in := gateInput("true")
	in.PrevResult = json.RawMessage(`{"message":"Fine. Authorization: Basic ZmFrZTpmYWtl","review":{"approved":true,"summary":"ok"}}`)
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if prov.commentBody == "" || strings.Contains(prov.commentBody, "ZmFrZTpmYWtl") {
		t.Fatalf("the withheld-approval comment: %q", prov.commentBody)
	}
}

func TestPublication838_TheProseReviewIsPinnedToItsHead(t *testing.T) {
	prov := post838(t, `{"body":"Looks good."}`, forgeapi.ForgeJob{Repo: "o/r", Number: 1, HeadSHA: "abc123"}, detector838(t))
	if prov.gotReview.CommitID != "abc123" {
		t.Fatalf("CommitID = %q, want the reviewed head", prov.gotReview.CommitID)
	}
}

// Overlapping spans (a detector finding inside a Basic Auth match) are masked
// whole: secrets.Redact skips a span that starts inside the previous one.
func TestPublicationFindings838_OverlapsAreJoined(t *testing.T) {
	text := "Authorization: Basic ZmFrZTpmYWtl and the end"
	spans := []secrets.Finding{{Type: "a", Start: 15, End: 33}, {Type: "b", Start: 21, End: 40}}
	got := joinSpans(spans)
	if len(got) != 1 || got[0].Start != 15 || got[0].End != 40 {
		t.Fatalf("joinSpans = %+v", got)
	}
	if out := string(secrets.Redact([]byte(text), got)); strings.Contains(out, "ZmFrZTpmYWtl") {
		t.Fatalf("redacted text still holds the value: %q", out)
	}
}

func TestPublicationFindings838_DetachedBase64OnlyWhenItDecodesToUserPass(t *testing.T) {
	d := detector838(t)
	if f := publicationFindings(d, "commit deadbeefcafebabe0123 and Username"); len(f) != 0 {
		t.Fatalf("ordinary tokens flagged: %+v", f)
	}
	if f := publicationFindings(d, "value ZmFrZTpmYWtl here"); len(f) != 1 {
		t.Fatalf("a base64 user:pass was not flagged: %+v", f)
	}
}

// #838 item 4: an empty full diff is refused, not reviewed.
func TestFetchDiff838_AnEmptyDiffIsRefused(t *testing.T) {
	for _, d := range [][]byte{{}, []byte("  \n\t\n")} {
		prov := &fakeProvider{diff: d}
		h := NewFetchDiffHandler(fakeResolver{p: prov})
		_, err := h.Execute(context.Background(), executor.SystemStepInput{
			Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4}), Step: &registry.WorkflowStep{}})
		if err == nil || !strings.Contains(err.Error(), "empty diff") {
			t.Fatalf("diff %q: err = %v, want an empty-diff refusal", d, err)
		}
	}
}

// Review 1d6a H2: a detached base64 credential's decoded form is masked
// wherever it appears, even with no "decoded as" phrase near it.
func TestPublicationFindings838_TheDecodedFormIsMaskedToo(t *testing.T) {
	text := "Header value ZmFrZTpmYWtl. The credentials are fake:fake, base64 above."
	out := string(secrets.Redact([]byte(text), publicationFindings(detector838(t), text)))
	if strings.Contains(out, "ZmFrZTpmYWtl") || strings.Contains(out, "fake:fake") {
		t.Fatalf("a form of the credential was published: %q", out)
	}
}

// Review 1d6a M5: URL-safe base64 is caught like the standard alphabet.
func TestPublicationFindings838_URLSafeBase64(t *testing.T) {
	// "us_er?:pa>ss" encodes with '-' and '_' in the URL alphabet.
	tok := "dXNfZXI_OnBhPnNz"
	if f := publicationFindings(detector838(t), "token "+tok+" here"); len(f) == 0 {
		t.Fatalf("a URL-safe base64 credential was not flagged")
	}
}

// Review 1d6a M6: the check run carries the redacted text, not the raw one.
func TestPublication838_TheCheckRunCarriesTheRedactedText(t *testing.T) {
	prov := &checkingProvider{fakeProvider: &fakeProvider{}}
	h := newTestPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	fetched, _ := json.Marshal(map[string]any{"diff": "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n x\n+y\n", "head_sha": "abc123"})
	verdict, _ := json.Marshal(map[string]any{"review": map[string]any{"approved": false,
		"summary":  "Fine overall.",
		"findings": []map[string]any{{"file": "a.go", "line": 2, "severity": "minor", "title": "t", "detail": "d"}}}})
	job := forgeapi.ForgeJob{Repo: "o/r", Number: 4, OnDemand: true, CommentAuthor: "m", CommentBody: "Authorization: Basic ZmFrZTpmYWtl"}
	in := executor.SystemStepInput{Task: taskWithJob(job), PrevResult: verdict,
		Step:        &registry.WorkflowStep{GroundFindings: true, CheckRun: true},
		StepResults: map[string]json.RawMessage{"fetch_diff": fetched}}
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(prov.runs) != 1 {
		t.Fatalf("check runs: %d", len(prov.runs))
	}
	raw, _ := json.Marshal(prov.runs[0])
	if strings.Contains(string(raw), "ZmFrZTpmYWtl") || strings.Contains(prov.gotReview.Body, "ZmFrZTpmYWtl") {
		t.Fatalf("the value reached the check run or the review: %s", raw)
	}
}

// A finding's severity is model text posted into its inline comment, so it is
// scanned with the title and detail (found while answering review 82a3 M2).
func TestPublication838_ASecretInASeverityWithholdsTheFinding(t *testing.T) {
	added, _ := parseAddedLines([]byte("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n x\n+y\n"))
	g := groundFindings([]reviewFinding{{File: "a.go", Line: 2, Severity: "Authorization: Basic ZmFrZTpmYWtl", Title: "t", Detail: "d"}},
		added, detector838(t))
	if len(g.Kept) != 0 || g.Echoed != 1 {
		t.Fatalf("a secret in the severity was kept: %+v", g)
	}
}

// Review 82a3 L1: a decoded "password" under 3 characters is not a credential
// (a ratio like 1:2 must not mask every ratio in the review).
func TestPublicationFindings838_ShortDecodedPairsAreNotCredentials(t *testing.T) {
	// "YWJjOmRl" decodes to "abc:de": 8 base64 characters, a 2-character password.
	if f := publicationFindings(detector838(t), "value YWJjOmRl here and 1:2 elsewhere"); len(f) != 0 {
		t.Fatalf("a 2-character password was taken for a credential: %+v", f)
	}
}
