package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

// Trading ingest dropped-field detector (trading-fill-reconciliation design,
// finding #7 correction, 2026-09-24). The broker's `filled_qty` reached the
// daemon on every status row for months and encoding/json discarded it,
// because the ingest struct declared no such field — silently. Each trading
// ingest now reports any body key its struct does not declare: a counter,
// and one WARN per (endpoint, key) for the life of the process. It never
// refuses the row: the audit pipe is fire-and-forget, and refusing would turn
// a newer broker's extra field into lost orders.

// ingestKnownKeys caches each request type's declared json keys.
var ingestKnownKeys sync.Map // reflect.Type -> map[string]bool

// declaredJSONKeys returns the json keys a struct type decodes, following
// embedded structs the way encoding/json does.
func declaredJSONKeys(t reflect.Type) map[string]bool {
	if v, ok := ingestKnownKeys.Load(t); ok {
		return v.(map[string]bool)
	}
	keys := map[string]bool{}
	collectJSONKeys(t, keys)
	ingestKnownKeys.Store(t, keys)
	return keys
}

func collectJSONKeys(t reflect.Type, keys map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			collectJSONKeys(f.Type, keys)
			continue
		}
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		keys[name] = true
	}
}

// noteUnknownIngestFields reports the keys of body that req's type does not
// declare. body has already decoded into req, so it is a JSON object; a
// decode failure here is ignored (the caller has validated the body).
func (s *Server) noteUnknownIngestFields(endpoint string, body []byte, req any) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return
	}
	known := declaredJSONKeys(reflect.TypeOf(req).Elem())
	for key := range raw {
		if known[key] {
			continue
		}
		if s.tradingMetrics != nil && s.tradingMetrics.IngestUnknownFieldsTotal != nil {
			s.tradingMetrics.IngestUnknownFieldsTotal.WithLabelValues(endpoint, key).Inc()
		}
		if _, seen := s.ingestUnknownSeen.LoadOrStore(endpoint+"\x00"+key, true); !seen {
			s.logger.Warn().Str("endpoint", endpoint).Str("field", key).
				Msg("trading ingest: the broker sent a field this daemon does not store; it is dropped — upgrade the daemon or map the field")
		}
	}
}
