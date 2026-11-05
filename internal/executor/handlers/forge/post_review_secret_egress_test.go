package forge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
)

func TestPostReview_SecretBodyIsNeverPublished(t *testing.T) {
	cases := []struct {
		name string
		prev string
		job  forgeapi.ForgeJob
	}{
		{
			name: "raw fallback basic header",
			prev: `{"body":"Authorization: Basic ZmFrZTpmYWtl"}`,
			job:  forgeapi.ForgeJob{Repo: "o/r", Number: 1},
		},
		{
			name: "raw fallback detached basic value",
			prev: `{"body":"The encoded value was ZmFrZTpmYWtl"}`,
			job:  forgeapi.ForgeJob{Repo: "o/r", Number: 1},
		},
		{
			name: "structured feedback decoded basic value",
			prev: `{"review":{"feedback":"Decoded as example_user:fake-passphrase"}}`,
			job:  forgeapi.ForgeJob{Repo: "o/r", Number: 1},
		},
		{
			name: "structured summary credential assignment",
			prev: `{"review":{"summary":"password: fake-demo-value"}}`,
			job:  forgeapi.ForgeJob{Repo: "o/r", Number: 1},
		},
		{
			name: "quoted request",
			prev: `{"body":"Review looks good"}`,
			job: forgeapi.ForgeJob{
				Repo: "o/r", Number: 1, OnDemand: true,
				CommentAuthor: "maintainer", CommentBody: "Please include decoded as example_user:fake-passphrase in the answer",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{}
			input := executor.SystemStepInput{
				Task: taskWithJob(tc.job), Step: &registry.WorkflowStep{Handler: "forge.post_review"},
				PrevResult: json.RawMessage(tc.prev),
			}
			_, err := newTestPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).Execute(context.Background(), input)
			if err == nil {
				t.Fatal("expected secret-bearing body to be refused")
			}
			if prov.gotReview.Body != "" || prov.commentBody != "" {
				t.Fatal("provider was called for a secret-bearing body")
			}
			for _, value := range []string{"ZmFrZTpmYWtl", "example_user:fake-passphrase", "fake-demo-value"} {
				if strings.Contains(err.Error(), value) {
					t.Fatal("refusal error disclosed matched content")
				}
			}
		})
	}
}

func TestPostReview_SecretBodyIsBlockedOnWithheldCommentPath(t *testing.T) {
	prov := &fakeProvider{}
	h := gateHandler(prov, &gateOutcomes{outs: redRun()}, true)
	in := gateInput("true")
	in.PrevResult = json.RawMessage(`{"message":"Authorization: Basic ZmFrZTpmYWtl","review":{"approved":true,"summary":"password: fake-demo-value"}}`)
	if _, err := h.Execute(context.Background(), in); err == nil {
		t.Fatal("expected secret-bearing comment to be refused")
	}
	if prov.gotReview.Body != "" || prov.commentBody != "" {
		t.Fatal("provider was called for a secret-bearing withheld-approval comment")
	}
}

func TestPostReview_RequiresSecretDetector(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	in := executor.SystemStepInput{
		Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 1}),
		PrevResult: json.RawMessage(`{"body":"Looks good"}`),
	}
	if _, err := h.Execute(context.Background(), in); err == nil {
		t.Fatal("missing detector must refuse publication")
	}
	if prov.gotReview.Body != "" || prov.commentBody != "" {
		t.Fatal("provider was called without a secret detector")
	}
}

func TestPostReview_SafePublicIdentifiersStillPublish(t *testing.T) {
	prov := &fakeProvider{}
	h := newTestPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	in := executor.SystemStepInput{
		Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 1}),
		PrevResult: json.RawMessage(`{"body":"KMS ARN arn:aws:kms:eu-central-1:123456789012:key/123e4567-e89b-12d3-a456-426614174000; commit 0123456789abcdef0123456789abcdef01234567"}`),
	}
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatalf("safe public identifiers should not block a review: %v", err)
	}
	if prov.gotReview.Body == "" {
		t.Fatal("safe review was not published")
	}
}
