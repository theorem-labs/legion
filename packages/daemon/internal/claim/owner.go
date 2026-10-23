package claim

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxOwnerTokenLength bounds an owner token. A token names files under the state directory
// (`secrets/<token>-nats_nkey_seed`, `owners/<token>`, `prompts/<token>.md`), and a file name is at
// most 255 bytes (NAME_MAX); 200 leaves room for every suffix the daemon adds. An Envoy topic, the
// token's other use, allows far more (a NATS subject of 4032 bytes).
const MaxOwnerTokenLength = 200

// NodeKinds are the graph node kinds an owner key may name, the part of a node id before its
// first colon.
var NodeKinds = []string{"function", "struct", "global", "percpu", "subsystem"}

var (
	// nodeOwnerKey is a graph node id: a node kind, a colon, and a suffix with no whitespace
	// (`function:mnt_get_writers@fs/namespace.c`, `subsystem:vfs.core`).
	nodeOwnerKey = regexp.MustCompile(`^(?:` + strings.Join(NodeKinds, "|") + `):\S+$`)
	// namedOwnerKey is a named role (`librarian`, `escalations`).
	namedOwnerKey = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// IsOwnerKey reports whether key is one an owner may hold: a graph node id of a known kind, or a
// named role. A key is UTF-8 text: the TypeScript codec encodes a JavaScript string, which holds
// no lone byte, so a key with one would have a token only the Go side could spell.
func IsOwnerKey(key string) bool {
	return utf8.ValidString(key) && (nodeOwnerKey.MatchString(key) || namedOwnerKey.MatchString(key))
}

// ownerEscapes are the bytes an owner name spells as `_` and one letter; every other byte outside
// the literal alphabet (a-z, 0-9, `-`) but an uppercase letter is `_x` and two lowercase hex digits.
var ownerEscapes = map[byte]byte{'_': '_', '.': 'd', ':': 'c', '/': 's', '@': 'a'}

// ownerUnescapes is ownerEscapes reversed.
var ownerUnescapes = map[byte]byte{'_': '_', 'd': '.', 'c': ':', 's': '/', 'a': '@'}

// literalOwnerByte is a byte an owner name holds as itself.
func literalOwnerByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
}

const lowerHex = "0123456789abcdef"

// EncodeOwnerName spells an owner key in an Envoy role token's alphabet (`[a-z0-9_-]`): a-z, 0-9
// and `-` as themselves, `_` as `__`, `.` as `_d`, `:` as `_c`, `/` as `_s`, `@` as `_a`, an
// uppercase letter as `_u` and the letter lowercased, and any other byte as `_x` and its two
// lowercase hex digits. Every key has exactly one spelling, so DecodeOwnerName reverses it and two
// keys never share a name: `function:a_b` is `function_ca__b`, `function:a/b` `function_ca_sb`.
func EncodeOwnerName(key string) (string, error) {
	if !IsOwnerKey(key) {
		return "", fmt.Errorf("owner key %q is neither a graph node id (%s, a colon, then no whitespace) nor a named role (a lowercase letter, then lowercase letters, digits and -)",
			key, strings.Join(NodeKinds, "|"))
	}
	var out strings.Builder
	for i := range len(key) {
		c := key[i]
		switch {
		case literalOwnerByte(c):
			out.WriteByte(c)
		case ownerEscapes[c] != 0:
			out.WriteByte('_')
			out.WriteByte(ownerEscapes[c])
		case c >= 'A' && c <= 'Z':
			out.WriteString("_u")
			out.WriteByte(c - 'A' + 'a')
		default:
			out.WriteString("_x")
			out.WriteByte(lowerHex[c>>4])
			out.WriteByte(lowerHex[c&0xf])
		}
	}
	return out.String(), nil
}

// DecodeOwnerName is the owner key name spells (EncodeOwnerName). It is strict: a name that no key
// encodes to — a lone `_` at its end, an unknown escape letter, `_x` not followed by two lowercase
// hex digits, `_u` not followed by a lowercase letter, an `_x` escape of a byte that has a shorter
// spelling (`_x5f` for `_`, `_x2e` for `.`, `_x41` for `A`, `_x61` for `a`), or one that spells a
// key outside IsOwnerKey (`function_c`, `file_cx`, a space as `_x20`) — is refused, so no two names
// decode to one key.
func DecodeOwnerName(name string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if literalOwnerByte(c) {
			out.WriteByte(c)
			continue
		}
		if c != '_' {
			return "", fmt.Errorf("owner name %q holds %q at byte %d, outside an owner name's alphabet", name, c, i)
		}
		if i+1 >= len(name) {
			return "", fmt.Errorf("owner name %q ends in a lone _", name)
		}
		i++
		switch e := name[i]; e {
		case 'u':
			if i+1 >= len(name) || name[i+1] < 'a' || name[i+1] > 'z' {
				return "", fmt.Errorf("owner name %q has _u at byte %d with no lowercase letter after it", name, i-1)
			}
			i++
			out.WriteByte(name[i] - 'a' + 'A')
		case 'x':
			if i+2 >= len(name) {
				return "", fmt.Errorf("owner name %q has _x at byte %d without two hex digits after it", name, i-1)
			}
			high, low := strings.IndexByte(lowerHex, name[i+1]), strings.IndexByte(lowerHex, name[i+2])
			if high < 0 || low < 0 {
				return "", fmt.Errorf("owner name %q has _x at byte %d without two lowercase hex digits after it", name, i-1)
			}
			b := byte(high<<4 | low)
			if literalOwnerByte(b) || ownerEscapes[b] != 0 || b >= 'A' && b <= 'Z' {
				return "", fmt.Errorf("owner name %q spells %q at byte %d as _x%s, which has a shorter spelling", name, b, i-1, name[i+1:i+3])
			}
			i += 2
			out.WriteByte(b)
		default:
			unescaped, ok := ownerUnescapes[e]
			if !ok {
				return "", fmt.Errorf("owner name %q holds the unknown escape _%c at byte %d", name, e, i-1)
			}
			out.WriteByte(unescaped)
		}
	}
	key := out.String()
	if !IsOwnerKey(key) {
		return "", fmt.Errorf("owner name %q spells %q, which is not an owner key", name, key)
	}
	return key, nil
}
