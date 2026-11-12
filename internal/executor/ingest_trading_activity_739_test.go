package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// TestIngestTradingActivity_TheHandlersResultMakesTheAgentsChunk_739: the
// execute step's result moves from the executor model to
// trading.place_approved (GitHub #739). The strategist recalls the "recent
// fills" chunk built from it, so the handler's result (which adds intent,
// action, qty, limit and key to each placed item) must produce the same
// chunk as the agent's result did for the same orders.
func TestIngestTradingActivity_TheHandlersResultMakesTheAgentsChunk_739(t *testing.T) {
	agent := `{"placed":[{"symbol":"JPM","broker_order_id":"1234","status":"filled","idempotency_key":"jpm-open-20261111"}],` +
		`"skipped":[{"symbol":"BAC","reason":"price_moved","detail":"ask 40.3"}],` +
		`"fills_observed":[{"symbol":"JPM","qty":4,"price":300}]}`
	handler := `{"placed":[{"symbol":"JPM","action":"BUY","intent":"open","qty":4,"limit_price":300,` +
		`"idempotency_key":"vx1-72f04c500594c14735eaca39","broker_order_id":"1234","status":"filled"}],` +
		`"skipped":[{"symbol":"BAC","reason":"price_moved","detail":"ask 40.3"}],` +
		`"fills_observed":[{"symbol":"JPM","qty":4,"price":300}]}`

	chunk := func(result string) string {
		idx := &stubMemoryIndexer{}
		e := &Executor{logger: zerolog.Nop(), memoryIndexer: idx}
		e.ingestTradingActivity(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p1"},
			&persistence.Execution{ID: "exec1"}, []byte(result))
		require.Len(t, idx.calls, 1)
		// Drop the first line: it carries the wall-clock stamp.
		_, body, _ := strings.Cut(idx.calls[0].content, "\n")
		return body
	}
	assert.Equal(t, chunk(agent), chunk(handler))
	assert.Contains(t, chunk(handler), "- JPM: status=filled broker_id=1234")
}
