package registry

import "testing"

// Issue #71: document_render is a constrained in-container transform, so a
// broker role can explicitly hold it without an unrestricted execution grant.
func TestBrokerDocumentRenderIsSafeBuiltin(t *testing.T) {
	if !IsBrokerSafeBuiltin("document_render") {
		t.Fatal("document_render is unavailable to broker roles")
	}
	if err := checkBrokerTool(&Project{}, "writer", "document_render", nil); err != nil {
		t.Fatal(err)
	}
	if err := checkBrokerTool(&Project{}, "writer", "run_shell", nil); err == nil {
		t.Fatal("rendering must not grant shell access")
	}
}
