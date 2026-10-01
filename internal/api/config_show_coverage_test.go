package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
)

// credentialShapedName matches a config key NAME (its last yaml segment, or
// its Go field name) that plausibly carries a credential. It is broader than
// the redactor's own token list on purpose: the redactor is a denylist, and
// a denylist reports "clean" about every name not on it. This test is the
// denominator for that list — every credential-shaped leaf of config.Config
// is examined, and each one must either come back redacted from
// GET /api/v1/config or be on reviewedNonSecretLeaves with a reason.
var credentialShapedName = regexp.MustCompile(`(?i)key|pass|secret|token|cred|private`)

// reviewedNonSecretLeaves are credential-shaped leaves a human has looked at
// and judged not secret. Each entry says why. Adding a leaf here is a claim
// that its VALUE is safe to show any operator-scope caller.
var reviewedNonSecretLeaves = map[string]string{
	// File paths, not key material: tls.LoadX509KeyPair reads them.
	"node.relay.client_key":         "PEM file path",
	"node.relay_ingress.server_key": "PEM file path",
}

// TestGetConfig_EveryCredentialShapedLeafIsRedacted is the regression test
// for the 2026-10-01 external scan of swarms.vornik.io (finding F2): the
// config dump redacts by key-name token, admin.allowed_keys marshals as
// "AllowedKeys" (no json tag), "allowedkeys" contains none of the tokens, and
// both admin keys came back in plaintext to an operator-scope caller. The
// per-field tests could not see it because each pinned a field someone had
// already thought of. This one walks EVERY leaf.
func TestGetConfig_EveryCredentialShapedLeafIsRedacted(t *testing.T) {
	cfg := &config.Config{}
	sentinels := map[string]string{} // dotted key → sentinel planted there
	opaque := fillSentinels(reflect.ValueOf(cfg).Elem(), "", sentinels)
	// A leaf typed interface{}/any cannot be planted generically (there is no
	// concrete type to allocate), so the walk would report it clean without
	// having looked. config.Config has none; one appearing must fail here
	// rather than widen the blind spot silently.
	require.Empty(t, opaque, "interface-typed config leaves the sentinel walk cannot examine")

	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))
	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	rec := httptest.NewRecorder()
	server.GetConfig(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	var examined, leaked []string
	for key, sentinel := range sentinels {
		if !credentialShapedName.MatchString(lastSegment(key)) {
			continue
		}
		examined = append(examined, key)
		if _, ok := reviewedNonSecretLeaves[key]; ok {
			continue
		}
		if strings.Contains(body, sentinel) {
			leaked = append(leaked, key)
		}
	}
	sort.Strings(leaked)
	// Publish the denominator: a zero-leak result over zero examined leaves
	// would be a test that stopped looking, not a clean config.
	t.Logf("examined %d credential-shaped leaves of %d planted; %d reviewed non-secret",
		len(examined), len(sentinels), len(reviewedNonSecretLeaves))
	// 32 at introduction (2026-10-01). The floor sits just under it so a
	// filler regression that silently drops a branch of the tree fails here.
	require.GreaterOrEqual(t, len(examined), 30, "the walk stopped finding credential-shaped leaves; the sentinel filler is broken")
	require.Empty(t, leaked, "credential-shaped config leaves returned unredacted by GET /api/v1/config")

	// A reviewed entry that no longer names a real leaf is a stale review.
	for key := range reviewedNonSecretLeaves {
		_, ok := sentinels[key]
		require.True(t, ok, "reviewedNonSecretLeaves names %q, which is no longer a config leaf", key)
	}
}

func lastSegment(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

// fillSentinels plants a unique string in every string-typed leaf reachable
// through yaml-tagged fields (strings, string slices, string-valued maps, and
// those inside slices/maps of structs), allocating nil pointers on the way so
// sections the zero Config leaves nil are covered too. It returns the dotted
// keys of interface-typed leaves, which it cannot plant.
func fillSentinels(v reflect.Value, prefix string, out map[string]string) (opaque []string) {
	switch v.Kind() {
	case reflect.Interface:
		return []string{prefix}
	case reflect.Pointer:
		if v.IsNil() {
			if !v.CanSet() {
				return
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		return fillSentinels(v.Elem(), prefix, out)
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || !v.Field(i).CanSet() {
				continue
			}
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				if !f.Anonymous {
					continue
				}
				opaque = append(opaque, fillSentinels(v.Field(i), prefix, out)...) // inlined/embedded
				continue
			}
			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			opaque = append(opaque, fillSentinels(v.Field(i), key, out)...)
		}
	case reflect.String:
		s := fmt.Sprintf("S3NT1NEL-%04d-%s", len(out), prefix)
		v.SetString(s)
		out[prefix] = s
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		opaque = fillSentinels(elem, prefix, out)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return []string{prefix}
		}
		m := reflect.MakeMap(v.Type())
		elem := reflect.New(v.Type().Elem()).Elem()
		opaque = fillSentinels(elem, prefix+"[x]", out)
		m.SetMapIndex(reflect.ValueOf("x").Convert(v.Type().Key()), elem)
		v.Set(m)
	}
	return opaque
}
