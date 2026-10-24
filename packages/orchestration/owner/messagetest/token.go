package messagetest

import "strings"

// EncodeOwnerName is an owner id as the supervisor spells it in a claim token and a working directory (shared
// contract C9): `_`→`__`, `.`→`_d`, `:`→`_c`, `/`→`_s`, `@`→`_a`, an uppercase letter → `_u` and the letter in
// lowercase, and any other byte outside [a-z0-9-] → `_x` and two lowercase hex digits. The driver uses it to check
// that an owner pane works in the directory its claim names.
func EncodeOwnerName(id string) string {
	const hex = "0123456789abcdef"
	var out strings.Builder
	out.Grow(len(id) + len(id)/2)
	for index := range len(id) {
		c := id[index]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out.WriteByte(c)
		case c == '_':
			out.WriteString("__")
		case c == '.':
			out.WriteString("_d")
		case c == ':':
			out.WriteString("_c")
		case c == '/':
			out.WriteString("_s")
		case c == '@':
			out.WriteString("_a")
		case c >= 'A' && c <= 'Z':
			out.WriteString("_u")
			out.WriteByte(c - 'A' + 'a')
		default:
			out.WriteString("_x")
			out.WriteByte(hex[c>>4])
			out.WriteByte(hex[c&0x0f])
		}
	}
	return out.String()
}
