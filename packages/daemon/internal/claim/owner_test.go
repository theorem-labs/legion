package claim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ownerTokenFixture is packages/contracts/fixtures/owner-tokens.json, which the TypeScript codec's
// test (packages/contracts/src/legion-roles.test.ts) reads too: keys with their tokens, keys the
// grammar refuses, and names no key encodes to.
type ownerTokenFixture struct {
	Project   string            `json:"project"`
	Tokens    map[string]string `json:"tokens"`
	Refused   []string          `json:"refused"`
	Malformed []string          `json:"malformed"`
}

func readOwnerTokenFixture(t *testing.T) ownerTokenFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "owner-tokens.json"))
	if err != nil {
		t.Fatalf("read the owner-token fixture: %v", err)
	}
	var fixture ownerTokenFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the owner-token fixture: %v", err)
	}
	return fixture
}

// An owner token is the Envoy role its agent claims, so the plugin's standingOwnerToken spells it
// byte for byte as OwnerToken does, and each decodes it back to the one key it encodes.
func TestOwnerTokenIsTheShippedSpelling(t *testing.T) {
	fixture := readOwnerTokenFixture(t)
	for key, want := range fixture.Tokens {
		got, err := OwnerToken(fixture.Project, key)
		if err != nil || string(got) != want {
			t.Errorf("OwnerToken(%q) = %q, %v; want %q", key, got, err, want)
			continue
		}
		if back, ok := got.OwnerKey(fixture.Project); !ok || back != key {
			t.Errorf("%s.OwnerKey() = %q, %t; want %q", got, back, ok, key)
		}
	}
	for _, key := range fixture.Refused {
		if token, err := OwnerToken(fixture.Project, key); err == nil {
			t.Errorf("OwnerToken(%q) = %q, want a refusal", key, token)
		}
	}
	for _, name := range fixture.Malformed {
		if key, err := DecodeOwnerName(name); err == nil {
			t.Errorf("DecodeOwnerName(%q) = %q, want a refusal", name, key)
		}
	}
}

// Every printable ASCII byte in a node id's suffix encodes to a name that decodes back to it, in
// the Envoy role-token alphabet, and no two of them share a name.
func TestEveryPrintableByteRoundTripsAndStaysDistinct(t *testing.T) {
	seen := map[string]byte{}
	for c := byte(0x21); c < 0x7f; c++ {
		key := "function:a" + string(c)
		name, err := EncodeOwnerName(key)
		if err != nil {
			t.Fatalf("EncodeOwnerName(%q): %v", key, err)
		}
		if !envoyRoleToken.MatchString(name) {
			t.Fatalf("EncodeOwnerName(%q) = %q, outside the Envoy role-token alphabet", key, name)
		}
		if other, ok := seen[name]; ok {
			t.Fatalf("%q and %q share the name %q", key, "function:a"+string(other), name)
		}
		seen[name] = c
		if back, err := DecodeOwnerName(name); err != nil || back != key {
			t.Fatalf("DecodeOwnerName(%q) = %q, %v; want %q", name, back, err, key)
		}
	}
}

// Keys that differ only in a character a lossy rule would merge stay different tokens: an escape's
// letter written literally after an underscore is not the character it escapes.
func TestOwnerKeysThatDifferStayDistinct(t *testing.T) {
	seen := map[Token]string{}
	for _, key := range []string{
		"function:a_b", "function:a-b", "function:a.b", "function:aB", "function:ab", "function:a:b",
		"function:a/b", "function:a_sb", "function:a@b", "function:a_ab", "function:a_db", "function:a+b", "function:a_x2bb",
	} {
		token, err := OwnerToken("proj", key)
		if err != nil {
			t.Fatal(err)
		}
		if other, ok := seen[token]; ok {
			t.Fatalf("%q and %q share the token %s", key, other, token)
		}
		seen[token] = key
	}
}

// A token names files under the state directory, so it is bounded; the graph's longest node id
// fits with room to spare, and a key one byte past the bound is refused.
func TestOwnerTokensRespectTheirLengthLimit(t *testing.T) {
	longest := "function:do_attribute_container_device_trigger_safe@drivers/base/attribute_container.c"
	if token, err := OwnerToken("sjawharlegion", longest); err != nil || len(token) > MaxOwnerTokenLength {
		t.Fatalf("the graph's longest node id: %q (%d bytes), %v", token, len(token), err)
	}
	limit := MaxOwnerTokenLength - len("legion-p-owner-")
	if _, err := OwnerToken("p", strings.Repeat("a", limit)); err != nil {
		t.Fatalf("a token at the limit was refused: %v", err)
	}
	if _, err := OwnerToken("p", strings.Repeat("a", limit+1)); err == nil {
		t.Fatal("a token past the limit was accepted")
	}
}

// An owner token is read as an owner's before any workflow role, and a workflow token whose issue's
// project is OWNER is not read as one: its part after `owner-` decodes to no owner key.
func TestOwnerTokensAreTheOwnerRole(t *testing.T) {
	token, err := OwnerToken("proj", "function:mnt_get_writers@fs/namespace.c")
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := token.Role(); !ok || role != RoleOwner {
		t.Fatalf("%s.Role() = %q, %t; want owner", token, role, ok)
	}
	workflow, err := NewToken("proj", "OWNER-12", RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := workflow.Role(); !ok || role != RoleArchitect {
		t.Fatalf("%s.Role() = %q, %t; want architect", workflow, role, ok)
	}
}
