package messagetest

import "testing"

// The vectors follow shared contract C9: `_`→`__`, `.`→`_d`, `:`→`_c`, `/`→`_s`, `@`→`_a`, an uppercase letter →
// `_u` and the letter in lowercase, any other byte outside [a-z0-9-] → `_x` and two lowercase hex digits.
func TestEncodeOwnerNameFollowsTheSharedEncoding(t *testing.T) {
	for id, want := range map[string]string{
		"function:get_mountpoint@fs/namespace.c": "function_cget__mountpoint_afs_snamespace_dc",
		"struct:mountpoint":                      "struct_cmountpoint",
		"librarian":                              "librarian",
		"fact-stack-alignment":                   "fact-stack-alignment",
		"function:Foo":                           "function_c_ufoo",
		"function:a b":                           "function_ca_x20b",
		"function:\u00e9":                        "function_c_xc3_xa9",
		"global:x_":                              "global_cx__",
	} {
		if got := EncodeOwnerName(id); got != want {
			t.Errorf("EncodeOwnerName(%q) = %q, want %q", id, got, want)
		}
	}
}
