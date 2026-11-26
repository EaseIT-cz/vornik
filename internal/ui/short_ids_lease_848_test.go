package ui

// GitHub #848: the lease ID on the task page is a *string; inside `with`,
// shortID must still receive it and print L-xxxx, with the full ID in title.

import (
	"html/template"
	"strings"
	"testing"
)

func TestShortIDs848_ALeasePointerRendersShort(t *testing.T) {
	tpl := template.Must(template.New("t").Funcs(template.FuncMap{"shortID": shortID}).Parse(
		`<dd{{with .LeaseID}} title="{{.}}">{{shortID .}}{{else}}>{{end}}</dd>`))
	lease := "lease-0123456789abcdef0123456789abfee9"
	var b strings.Builder
	if err := tpl.Execute(&b, struct{ LeaseID *string }{&lease}); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != `<dd title="`+lease+`">L-fee9</dd>` {
		t.Fatalf("rendered %q", got)
	}
	b.Reset()
	if err := tpl.Execute(&b, struct{ LeaseID *string }{nil}); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != `<dd></dd>` {
		t.Fatalf("no lease rendered %q", got)
	}
}
