package claim

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxOwnerTokenLength bounds an owner token. A token names files under the state directory
// (`secrets/<token>-nats_nkey_seed`, `owners/<token>`, `prompts/<token>.md`), and a file name is at
// most 255 bytes (NAME_MAX); 200 leaves room for every suffix the daemon adds. An Envoy topic, the
// token's other use, allows far more (a NATS subject of 4032 bytes).
const MaxOwnerTokenLength = 200

// ownerKey is what an owner's ledger key may be: a graph node id (`function:get_mountpoint`,
// `struct:mount`, `global:mount_lock`, `subsystem:vfs.core`) or a named role (`librarian`,
// `owner-a`). It starts with a lowercase letter, so its encoding does too, and no workflow token,
// whose part after the project is an issue's lowercased Dispatch project, can read as one.
var ownerKey = regexp.MustCompile(`^[a-z][A-Za-z0-9_.:-]*$`)

// ownerEscapes are the key characters an Envoy role token (`[a-z0-9_-]`) cannot hold, each as `_`
// and one letter; an uppercase letter is `_u` and the letter lowercased.
var ownerEscapes = map[byte]byte{'_': '_', '.': 'd', ':': 'c'}

// EncodeOwnerName spells an owner key in an Envoy role token's alphabet: lowercase letters, digits
// and `-` as themselves, `_` as `__`, `.` as `_d`, `:` as `_c`, and an uppercase letter as `_u`
// and the letter lowercased. Every key has exactly one spelling, so DecodeOwnerName reverses it and
// two keys never share a name: `function:a_b` is `function_ca__b`, `function:a-b` is
// `function_ca-b`.
func EncodeOwnerName(key string) (string, error) {
	if !ownerKey.MatchString(key) {
		return "", fmt.Errorf("owner key %q must start with a lowercase letter and hold only letters, digits, and _ . : -", key)
	}
	var out strings.Builder
	for i := range len(key) {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out.WriteString("_u")
			out.WriteByte(c - 'A' + 'a')
		case ownerEscapes[c] != 0:
			out.WriteByte('_')
			out.WriteByte(ownerEscapes[c])
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), nil
}

// DecodeOwnerName is the owner key name spells (EncodeOwnerName). A name that is no key's spelling
// is refused.
func DecodeOwnerName(name string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_':
			if i+1 >= len(name) {
				return "", fmt.Errorf("owner name %q ends in an escape", name)
			}
			i++
			switch e := name[i]; {
			case e == 'u' && i+1 < len(name) && name[i+1] >= 'a' && name[i+1] <= 'z':
				i++
				out.WriteByte(name[i] - 'a' + 'A')
			case e == '_':
				out.WriteByte('_')
			case e == 'd':
				out.WriteByte('.')
			case e == 'c':
				out.WriteByte(':')
			default:
				return "", fmt.Errorf("owner name %q holds an unknown escape at byte %d", name, i)
			}
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out.WriteByte(c)
		default:
			return "", fmt.Errorf("owner name %q holds %q, outside an owner name's alphabet", name, c)
		}
	}
	key := out.String()
	if !ownerKey.MatchString(key) {
		return "", fmt.Errorf("owner name %q spells %q, which is not an owner key", name, key)
	}
	return key, nil
}
