package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf16"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// sealed_task_credentials.go — a captured credential is sealed at rest
// (GitHub #578; tool-credential-carryover design, Amendment 2026-10-31
// (#578) B1). Every reader and the writer go through this one type.

// OperatorSealer seals and opens values in the secret store's operator domain
// (#571 A1); *secretstore.Store implements it.
type OperatorSealer interface {
	SealOperator(label string, plain []byte) (string, error)
	OpenOperator(label, sealed string) ([]byte, error)
}

// SealedTaskCredentials seals Value on Upsert and opens it on read. A value
// that does not open for its row (unsealed, or sealed for another row) is left
// out of the list and logged, never returned.
type SealedTaskCredentials struct {
	inner  persistence.TaskCredentialRepository
	sealer func() (OperatorSealer, error)
	logger zerolog.Logger
}

// NewSealedTaskCredentials wraps inner. sealer is called per use, so a key
// minted after construction (secretStoreForWrite) is picked up.
func NewSealedTaskCredentials(inner persistence.TaskCredentialRepository, sealer func() (OperatorSealer, error), logger *zerolog.Logger) *SealedTaskCredentials {
	l := zerolog.Nop()
	if logger != nil {
		l = *logger
	}
	return &SealedTaskCredentials{inner: inner, sealer: sealer, logger: l}
}

// TaskCredentialLabel binds a sealed value to its row: the unique key's four
// fields, each percent-escaped so no segment can forge a boundary.
func TaskCredentialLabel(c *persistence.TaskCredential) string {
	return "TASKCRED/" + url.PathEscape(c.TaskID) + "/" + url.PathEscape(c.ExecutionID) + "/" +
		url.PathEscape(c.Tool) + "/" + url.PathEscape(c.ArtifactURL)
}

// Upsert seals the value and stores a copy; the caller's struct is unchanged.
func (r *SealedTaskCredentials) Upsert(ctx context.Context, c *persistence.TaskCredential) error {
	s, err := r.sealer()
	if err != nil {
		return err
	}
	sealed, err := s.SealOperator(TaskCredentialLabel(c), []byte(c.Value))
	if err != nil {
		return err
	}
	cp := *c
	cp.Value = sealed
	return r.inner.Upsert(ctx, &cp)
}

// ListByTaskLatestExecution returns the latest execution's credentials with
// their values opened.
func (r *SealedTaskCredentials) ListByTaskLatestExecution(ctx context.Context, taskID string) ([]*persistence.TaskCredential, error) {
	rows, err := r.inner.ListByTaskLatestExecution(ctx, taskID)
	if err != nil || len(rows) == 0 {
		return rows, err
	}
	s, err := r.sealer()
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, c := range rows {
		plain, err := s.OpenOperator(TaskCredentialLabel(c), c.Value)
		if err != nil {
			r.logger.Warn().Err(err).Str("credential_id", c.ID).Str("task_id", c.TaskID).
				Msg("task credential does not open for its row; not shown")
			continue
		}
		c.Value = string(plain)
		out = append(out, c)
	}
	return out, nil
}

// CarriedCredentialMarker replaces a carried credential in the tool audit.
const CarriedCredentialMarker = "[REDACTED:carried_credential]"

// CarriedCredentialForms is every form a carried value takes in stored tool
// output: the value itself and its JSON-escaped forms, with and without HTML
// escaping (a tool's JSON output may use either), each also with non-ASCII
// written as backslash-u escapes (Python's json.dumps default). Commit
// security review 2026-10-31: a value with a quote, backslash or angle
// bracket was missed.
func CarriedCredentialForms(value string) []string {
	forms := []string{value}
	for _, html := range []bool{true, false} {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		if err := enc.Encode(value); err != nil {
			continue
		}
		f := strings.TrimSuffix(strings.TrimSpace(b.String()), `"`)
		f = strings.TrimPrefix(f, `"`)
		for _, g := range []string{f, asciiEscaped(f)} {
			if g != "" && !containsString(forms, g) {
				forms = append(forms, g)
			}
		}
	}
	return forms
}

// asciiEscaped writes every non-ASCII rune of a JSON string body as a
// lowercase backslash-u escape, a UTF-16 surrogate pair above the BMP: the
// form a Python MCP server's json.dumps writes by default (ensure_ascii).
func asciiEscaped(s string) string {
	var b strings.Builder
	esc := func(r rune) { b.WriteString("\\" + fmt.Sprintf("u%04x", r)) }
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			esc(hi)
			esc(lo)
		default:
			esc(r)
		}
	}
	return b.String()
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
