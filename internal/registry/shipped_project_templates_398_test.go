package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// GitHub #398: configs/projects/trading-paper.yaml.template carried
// autonomy.interval, a key the strict loader rejects (the key is
// pollInterval), so a project copied from it, as https://docs.vornik.io tells
// an operator to do, was skipped at load. Every project template shipped under
// configs/projects/ must decode through the loader's own strict decoder and
// validate.
//
// Found at: review. A user observes it at integration (project load).
// Regression test at: integration (the loader's decoder over the shipped files).
func TestShippedProjectTemplates398_DecodeStrictAndValidate(t *testing.T) {
	files, err := filepath.Glob("../../configs/projects/*.yaml.template")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no project templates found under configs/projects; the glob or the tree moved")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		p, err := decodeProject(data)
		if err != nil {
			t.Errorf("%s: the strict loader rejects it: %v", filepath.Base(f), err)
			continue
		}
		if err := p.Validate(f); err != nil {
			t.Errorf("%s: validation: %v", filepath.Base(f), err)
		}
	}
	t.Logf("examined %d project templates", len(files))
}
