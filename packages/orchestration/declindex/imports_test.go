package declindex

import (
	"reflect"
	"testing"
)

func TestHeaderImports(t *testing.T) {
	src := []byte("/- doc -/\nprelude\nimport Lib.A\npublic import Lib.B Lib.C -- two\n\nnamespace X\nimport Lib.D\n")
	if got, want := HeaderImports(src), []string{"Lib.A", "Lib.B", "Lib.C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("HeaderImports = %v, want %v", got, want)
	}
}
