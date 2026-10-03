package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// The egress error classes a broker answer can fail with (broker design
// §5.1). An answer that was not written at all is the caller's to report.
const (
	EgressClassOversize = "egress_oversize"
	EgressClassSchema   = "egress_schema"
)

// EgressVerdict is ValidateBrokerEgress's judgement of one answer document.
type EgressVerdict struct {
	// Doc is the decoded answer when it is valid.
	Doc any
	// Class is "" for a valid answer, else EgressClassOversize or
	// EgressClassSchema.
	Class string
	// Messages say what is wrong, one per failure, in terms that never quote
	// a value the answer wrote (broker design §17, review 97b9 F2).
	Messages []string
	// Bytes and Limit are set for an oversize answer.
	Bytes, Limit int
}

// ValidateBrokerEgress judges a broker workflow's answer: the byte cap, JSON
// decoding, then the approved schema. It is the one implementation both the
// executor's answering-step check and result() use (broker design §17); each
// caller chooses which bytes to judge.
func ValidateBrokerEgress(wf *Workflow, body []byte) EgressVerdict {
	limit := wf.Broker.Egress.EffectiveMaxBytes()
	if len(body) > limit {
		return EgressVerdict{Class: EgressClassOversize, Bytes: len(body), Limit: limit,
			Messages: []string{fmt.Sprintf("the answer is %d bytes, above the %d-byte limit", len(body), limit)}}
	}
	schema, err := cachedEgressSchema(wf)
	if err != nil {
		return EgressVerdict{Class: EgressClassSchema, Messages: []string{"the approved schema does not compile"}}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return EgressVerdict{Class: EgressClassSchema, Messages: []string{"the answer is not one valid JSON document"}}
	}
	if dec.More() {
		return EgressVerdict{Class: EgressClassSchema, Messages: []string{"the answer has data after its JSON document"}}
	}
	if err := schema.Validate(doc); err != nil {
		return EgressVerdict{Class: EgressClassSchema, Messages: egressMessages(err)}
	}
	return EgressVerdict{Doc: doc}
}

var egressSchemas sync.Map // workflow id + schema hash -> *jsonschema.Schema

func cachedEgressSchema(wf *Workflow) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(wf.Broker.Egress.Schema)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	key := wf.ID + "@" + hex.EncodeToString(sum[:])
	if s, ok := egressSchemas.Load(key); ok {
		return s.(*jsonschema.Schema), nil
	}
	s, err := CompileBrokerSchema(wf.ID+"-egress", wf.Broker.Egress.Schema)
	if err != nil {
		return nil, err
	}
	egressSchemas.Store(key, s)
	return s, nil
}

// countingKeywords are the keywords whose bound and measured count may be
// stated: counts are structure, not content.
var countingKeywords = map[string]bool{"maxLength": true, "minLength": true, "maxItems": true, "minItems": true}

// nameKeywords state property names: the schema's own (required) or the
// keys the answer used (additionalProperties), never a value.
var nameKeywords = map[string]bool{"required": true, "additionalProperties": true}

var numberRE = regexp.MustCompile(`[0-9]+`)

// egressMessages renders each leaf failure from its instance pointer and
// keyword. The library's message text is used only to read the numbers of a
// counting keyword and the names of a name keyword, because for other
// keywords (format, pattern, enum, const, type, maximum, …) it can quote the
// value itself.
func egressMessages(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{"the answer does not match the approved schema"}
	}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		kw := e.KeywordLocation
		if i := strings.LastIndex(kw, "/"); i >= 0 {
			kw = kw[i+1:]
		}
		where := e.InstanceLocation
		if where == "" {
			where = "/"
		}
		switch {
		case countingKeywords[kw]:
			if n := numberRE.FindAllString(e.Message, -1); len(n) == 2 {
				out = append(out, fmt.Sprintf("%s: %s (%s allowed, %s written)", where, kw, n[0], n[1]))
				return
			}
			out = append(out, where+": "+kw)
		case nameKeywords[kw]:
			out = append(out, where+": "+kw+" ("+e.Message+")")
		default:
			out = append(out, where+": "+kw)
		}
	}
	walk(ve)
	sort.Strings(out)
	return out
}
