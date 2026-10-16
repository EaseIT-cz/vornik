package api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/registry"
)

// GitHub #407 unit level: the gate's outcome classification and its counter.
// The incident-level test is webhook_repo_scope_407_test.go.

func TestWebhookRepoScopeOutcome(t *testing.T) {
	scoped := registry.WebhookRepoScope{Repos: []string{"acme/own"}}
	assert.Equal(t, webhookRepoScopeAllowed, webhookRepoScopeOutcome(scoped, "ACME/own"))
	assert.Equal(t, webhookRepoScopeDropped, webhookRepoScopeOutcome(scoped, "acme/other"))
	assert.Equal(t, webhookRepoScopeNoRepository, webhookRepoScopeOutcome(scoped, ""))
	assert.Equal(t, webhookRepoScopeUnrestricted, webhookRepoScopeOutcome(registry.WebhookRepoScope{}, "acme/other"))
	assert.Equal(t, webhookRepoScopeUnrestricted, webhookRepoScopeOutcome(registry.WebhookRepoScope{ExplicitAll: true}, ""))
}

func TestRecordWebhookRepoScope_CountsEachOutcome(t *testing.T) {
	m := NewAPIMetrics(prometheus.NewRegistry())
	for _, o := range []string{webhookRepoScopeAllowed, webhookRepoScopeDropped, webhookRepoScopeDropped, webhookRepoScopeNoRepository, webhookRepoScopeUnrestricted} {
		m.RecordWebhookRepoScope("hm", "github", o)
	}
	assert.Equal(t, 2.0, testutil.ToFloat64(m.WebhookRepoScopeTotal.WithLabelValues("hm", "github", webhookRepoScopeDropped)))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.WebhookRepoScopeTotal.WithLabelValues("hm", "github", webhookRepoScopeAllowed)))
	assert.Equal(t, 4, testutil.CollectAndCount(m.WebhookRepoScopeTotal), "one series per outcome")
	var nilMetrics *APIMetrics
	nilMetrics.RecordWebhookRepoScope("hm", "github", webhookRepoScopeAllowed) // nil-safe
}
