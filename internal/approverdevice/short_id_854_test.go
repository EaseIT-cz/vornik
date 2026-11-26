package approverdevice

import (
	"html/template"
	"strings"
	"testing"
)

// GitHub #854 (UI design §24): the phone pages' device label comes from
// idfmt.Short now, so the console prints the same one; for a real device id
// it is unchanged ("dev_…" and the last 6 characters).
func TestDeviceLabel854_SameOnThePhoneAsBefore(t *testing.T) {
	tpl, err := template.New("label854").Funcs(pageFuncs).Parse(`{{shortID .}}`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newID("dev_")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tpl.Execute(&b, id); err != nil {
		t.Fatal(err)
	}
	if want := "dev_…" + id[len(id)-6:]; b.String() != want {
		t.Fatalf("device label = %q, want %q", b.String(), want)
	}
}
