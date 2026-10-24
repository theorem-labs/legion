package owner

import "testing"

func TestPathsTemplates(t *testing.T) {
	paths := Paths{
		EdgeModuleTemplate:       "Linux/Skeleton/{key_source}/E{hex16_target}.lean",
		EdgeTheoremTemplate:      "Skeleton.{key_source}.E{hex16_target}.edge",
		InterfaceModuleTemplate:  "Linux/Skeleton/{key_ds}/I{hex16_interface}.lean",
		InterfaceTheoremTemplate: "Skeleton.{key_ds}.I{hex16_interface}.edge",
		NodeModuleTemplate:       "Linux/Skeleton/{key}/Node.lean",
		EdgeRecordTemplate:       "skeleton/nodes/{key_source}/edges/E{hex16_target}.yaml",
		AgreementIDTemplate:      "edge:{source}->{target}@{n}",
	}
	if got, want := paths.EdgeModule("function:ns_match", "struct:ns_common"), "Linux/Skeleton/Kb401aaa780da5ad4/E9c01e86f8749cad3.lean"; got != want {
		t.Errorf("EdgeModule = %q, want %q", got, want)
	}
	if got, want := paths.EdgeTheorem("function:ns_match", "struct:ns_common"), "Skeleton.Kb401aaa780da5ad4.E9c01e86f8749cad3.edge"; got != want {
		t.Errorf("EdgeTheorem = %q, want %q", got, want)
	}
	if got, want := paths.InterfaceModule("struct:ns_common", "NsCommonIf"), "Linux/Skeleton/K9c01e86f8749cad3/I141cd37876b89b98.lean"; got != want {
		t.Errorf("InterfaceModule = %q, want %q", got, want)
	}
	if got, want := paths.InterfaceTheorem("struct:ns_common", "NsCommonIf"), "Skeleton.K9c01e86f8749cad3.I141cd37876b89b98.edge"; got != want {
		t.Errorf("InterfaceTheorem = %q, want %q", got, want)
	}
	if got, want := paths.NodeModule("function:ns_match"), "Linux/Skeleton/Kb401aaa780da5ad4/Node.lean"; got != want {
		t.Errorf("NodeModule = %q, want %q", got, want)
	}
	if got, want := paths.EdgeRecord("function:ns_match", "struct:ns_common"), "skeleton/nodes/Kb401aaa780da5ad4/edges/E9c01e86f8749cad3.yaml"; got != want {
		t.Errorf("EdgeRecord = %q, want %q", got, want)
	}
	if got, want := paths.AgreementID("function:ns_match", "struct:ns_common", 2), "edge:function:ns_match->struct:ns_common@2"; got != want {
		t.Errorf("AgreementID = %q, want %q", got, want)
	}
}
