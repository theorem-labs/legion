package landing

import (
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

func TestGraphFindingLineRoundTrips(t *testing.T) {
	f := Finding{Verdict: VerdictRefuse, Kind: blame.Unknown, Declaration: "Lib.Core.helper", Path: "Lib/Use.lean",
		Line: 5, Col: 36, Reason: "N1: Lib/Core.lean no longer declares Lib.Core.helper publicly"}
	line, err := FormatGraphFinding(f)
	if err != nil {
		t.Fatal(err)
	}
	if want := "refuse\tunknown\tLib.Core.helper\tLib/Use.lean:5:36\tN1: Lib/Core.lean no longer declares Lib.Core.helper publicly"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	if got, err := ParseGraphFinding(line); err != nil || got != f {
		t.Fatalf("ParseGraphFinding = %+v, %v", got, err)
	}
	for _, bad := range []string{
		"refuse\tunknown\tX\tA.lean:0:1\tN1: x",
		"maybe\tunknown\tX\tA.lean:1:1\tN1: x",
		"refuse\tweak\tX\tA.lean:1:1\tN1: x",
		"refuse\tunknown\tX\tA.lean:1:1",
		"refuse\tunknown\t\tA.lean:1:1\tN1: x",
		"refuse\tunknown\tX\t:1:1\tN1: x",
		"refuse\tunknown\tX\tA.lean:1\tN1: x",
		"refuse\tunknown\tX\tA.lean:1:1\t",
	} {
		if _, err := ParseGraphFinding(bad); err == nil {
			t.Fatalf("ParseGraphFinding(%q) accepted it", bad)
		}
	}
}

func TestFormatGraphFindingRefusesWhatParseRefuses(t *testing.T) {
	good := Finding{Verdict: VerdictFlag, Kind: blame.Changed, Declaration: "A.b", Path: "A.lean", Line: 1, Col: 1, Reason: "S1: x"}
	for name, bad := range map[string]func(*Finding){
		"verdict":     func(f *Finding) { f.Verdict = "maybe" },
		"weak kind":   func(f *Finding) { f.Kind = blame.Weak },
		"declaration": func(f *Finding) { f.Declaration = "" },
		"path":        func(f *Finding) { f.Path = "" },
		"line":        func(f *Finding) { f.Line = 0 },
		"col":         func(f *Finding) { f.Col = 0 },
		"tab":         func(f *Finding) { f.Reason = "S1: a\tb" },
		"newline":     func(f *Finding) { f.Declaration = "A.\nb" },
	} {
		f := good
		bad(&f)
		if line, err := FormatGraphFinding(f); err == nil {
			t.Errorf("%s: FormatGraphFinding wrote %q", name, line)
		}
	}
	if _, err := FormatGraphFinding(good); err != nil {
		t.Fatalf("FormatGraphFinding(%+v) = %v", good, err)
	}
}
