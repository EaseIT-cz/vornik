package mcpauth

import (
	"strings"
	"testing"
)

// GitHub #566 follow-up (design §16.5 C1, C2): the router provider's name and
// a manual endpoint's issuer are validated with the auth block.
func TestAuthValidate566_RouterAndIssuerFields(t *testing.T) {
	manual := Auth{Mode: ModeOAuth, AuthorizationEndpoint: "https://as.example/a", TokenEndpoint: "https://as.example/t"}
	for _, ok := range []Auth{
		{Mode: ModeOAuth, RouterProvider: "google"},
		func() Auth { a := manual; a.Issuer = "https://as.example"; return a }(),
		func() Auth { a := manual; a.Issuer, a.IssParameterSupported = "https://as.example", true; return a }(),
	} {
		if err := ok.Validate("streamable-http"); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for want, bad := range map[string]Auth{
		"router_provider":              {Mode: ModeOAuth, RouterProvider: "Google Inc"},
		"applies to a manual endpoint": {Mode: ModeOAuth, Issuer: "https://as.example"},
		"needs issuer":                 func() Auth { a := manual; a.IssParameterSupported = true; return a }(),
		"must be https":                func() Auth { a := manual; a.Issuer = "http://as.example"; return a }(),
		"not valid for mode":           {Mode: ModeStatic, Header: "X-Key", ValueFrom: "secret://k", RouterProvider: "google"},
	} {
		if err := bad.Validate("streamable-http"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err %v; want one naming %q", bad, err, want)
		}
	}
}
