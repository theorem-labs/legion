package declindex

import (
	"fmt"
	"regexp"
	"strings"
)

// FormsModuleName is the module that holds the registry's forms, compiled against the project's build.
const FormsModuleName = "DeclIndexForms"

// ConceptForms is one registered concept as the matcher sees it: its name (a Lean declaration of the project,
// or the name it will have) and the forms that state its fact, Lean lambda terms whose binders are pattern
// variables. A concept with no form is matched by its own declaration's body.
type ConceptForms struct {
	Concept string
	Forms   []string
}

// FormsModule is the Lean module that elaborates the forms with the project's own notation: each form is one
// definition, so `lean` elaborates it as the project's files are, and the exporter reads the compiled terms.
type FormsModule struct {
	Imports  []string
	Opens    []string
	Concepts []ConceptForms
}

// commandStart matches a line that starts a Lean command: a form must be one term, so it cannot contain one.
var commandStart = regexp.MustCompile(`(?m)^\s*(end|namespace|section|import|open|def|theorem|lemma|abbrev|instance|` +
	`structure|class|inductive|axiom|example|attribute|macro|syntax|elab|notation|set_option|initialize|` +
	`builtin_initialize|run_cmd|run_meta|#\w+)\b`)

// Check refuses a form that is not one lambda term.
func (f FormsModule) Check() error {
	for _, c := range f.Concepts {
		for _, form := range c.Forms {
			s := strings.TrimSpace(form)
			if !strings.HasPrefix(s, "fun ") && !strings.HasPrefix(s, "fun(") && !strings.HasPrefix(s, "λ") {
				return fmt.Errorf("form of %s is not a lambda term: %q", c.Concept, form)
			}
			if m := commandStart.FindString(s); m != "" || strings.Contains(s, "#eval") {
				return fmt.Errorf("form of %s contains a command (%q): a form is one term", c.Concept, strings.TrimSpace(m))
			}
		}
	}
	return nil
}

// Source is the module's text, and the constants the exporter takes as forms: one definition per written form,
// and a concept's own declaration for a concept with no form.
func (f FormsModule) Source() (string, []ConstRef) {
	var b strings.Builder
	for _, i := range f.Imports {
		fmt.Fprintf(&b, "import %s\n", i)
	}
	b.WriteString("\n")
	for _, o := range f.Opens {
		fmt.Fprintf(&b, "open %s\n", o)
	}
	b.WriteString("\nnamespace " + FormsModuleName + "\n\n")
	var refs []ConstRef
	for i, c := range f.Concepts {
		if len(c.Forms) == 0 {
			refs = append(refs, ConstRef{Concept: c.Concept, Const: c.Concept})
			continue
		}
		for j, form := range c.Forms {
			name := fmt.Sprintf("form_%d_%d", i, j)
			fmt.Fprintf(&b, "/-- A form of %s. -/\nnoncomputable def «%s» :=\n", c.Concept, name)
			for _, line := range strings.Split(strings.TrimSpace(form), "\n") {
				b.WriteString("  " + line + "\n")
			}
			b.WriteString("\n")
			refs = append(refs, ConstRef{Concept: c.Concept, Const: FormsModuleName + "." + name})
		}
	}
	b.WriteString("end " + FormsModuleName + "\n")
	return b.String(), refs
}
