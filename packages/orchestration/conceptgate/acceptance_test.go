package conceptgate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/internal/indextest"
)

// A librarian concept with forms and no stored source states its fact through its forms; the index need not
// declare its name (README: "A concept with no form is matched by its declaration's body", so one with forms is
// not). Forms keeps such a concept's forms, so that `declindex states` finds the statements that state its fact
// even when no module declares the concept.
func TestFormsKeepsUndeclaredConceptsForms(t *testing.T) {
	reg, defects, err := Load(filepath.Join("testdata", "registry"))
	if err != nil || len(defects) != 0 {
		t.Fatalf("load: %v %v", err, defects)
	}
	s, err := declindex.Open(context.Background(), declindex.Target{DSN: indextest.DSN(t), Index: "idx"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fm, err := Forms(context.Background(), s, reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fm.Concepts {
		if c.Concept == "Fx.SpAligned" && len(c.Forms) == 1 {
			return
		}
	}
	t.Fatalf("the forms module lost Fx.SpAligned's form; concepts %+v", fm.Concepts)
}
