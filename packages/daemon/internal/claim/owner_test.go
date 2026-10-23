package claim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An owner token is the Envoy role its agent claims, so the plugin's standingOwnerToken
// (packages/contracts/src/legion-roles.ts) spells it byte for byte as OwnerToken does. Both read
// packages/contracts/fixtures/owner-tokens.json.
func TestOwnerTokenIsTheShippedSpelling(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "owner-tokens.json"))
	if err != nil {
		t.Fatalf("read the owner-token fixture: %v", err)
	}
	var fixture struct {
		Project string            `json:"project"`
		Tokens  map[string]string `json:"tokens"`
		Refused []string          `json:"refused"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the owner-token fixture: %v", err)
	}
	for key, want := range fixture.Tokens {
		got, err := OwnerToken(fixture.Project, key)
		if err != nil || string(got) != want {
			t.Errorf("OwnerToken(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
	for _, key := range fixture.Refused {
		if token, err := OwnerToken(fixture.Project, key); err == nil {
			t.Errorf("OwnerToken(%q) = %q, want a refusal", key, token)
		}
	}
}

func TestOwnerKeysRoundTripThroughTokens(t *testing.T) {
	for _, key := range []string{
		"function:get_mountpoint", "struct:mount", "global:mount_lock", "subsystem:vfs.core",
		"function:Get_MountPoint", "librarian", "owner-a", "function:a_b", "function:a-b", "struct:x.y_Z9",
	} {
		token, err := OwnerToken("proj", key)
		if err != nil {
			t.Fatalf("OwnerToken(%q): %v", key, err)
		}
		if role, ok := token.Role(); !ok || role != RoleOwner {
			t.Fatalf("%s.Role() = %q, %t; want owner", token, role, ok)
		}
		if !envoyRoleToken.MatchString(string(token)) {
			t.Fatalf("token %q is not an Envoy role token", token)
		}
		got, ok := token.OwnerKey("proj")
		if !ok || got != key {
			t.Fatalf("%s.OwnerKey() = %q, %t; want %q", token, got, ok, key)
		}
	}
}

// Keys that differ only in a character a lossy rule would merge stay different tokens.
func TestOwnerKeysThatDifferStayDistinct(t *testing.T) {
	seen := map[Token]string{}
	for _, key := range []string{"function:a_b", "function:a-b", "function:a.b", "function:aB", "function:ab", "function:a:b"} {
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

func TestOwnerTokensRespectTheirLengthLimit(t *testing.T) {
	// The longest node id the graph has, and the worst case of escaping: every character doubles.
	worst := "struct:" + strings.Repeat("_", 34)
	if len(worst) != 41 {
		t.Fatalf("fixture is %d bytes", len(worst))
	}
	if token, err := OwnerToken("sjawharlegion", worst); err != nil || len(token) > MaxOwnerTokenLength {
		t.Fatalf("a 41-byte node id: %q (%d bytes), %v", token, len(token), err)
	}
	limit := MaxOwnerTokenLength - len("legion-p-owner-")
	if _, err := OwnerToken("p", strings.Repeat("a", limit)); err != nil {
		t.Fatalf("a token at the limit was refused: %v", err)
	}
	if _, err := OwnerToken("p", strings.Repeat("a", limit+1)); err == nil {
		t.Fatal("a token past the limit was accepted")
	}
}

func TestOwnerKeysOutsideTheAlphabetAreRefused(t *testing.T) {
	for _, key := range []string{"", ":x", "-x", "Function:x", "function:a b", "function:a/b", "function:é"} {
		if _, err := OwnerToken("proj", key); err == nil {
			t.Errorf("OwnerToken(%q) was accepted", key)
		}
	}
	for _, name := range []string{"-a", "4a", "A", "a.b", "a:b", "a_", "a_x", "a_u", "a_uA", "a_u4", "_c", "", "a_h"} {
		if _, err := DecodeOwnerName(name); err == nil {
			t.Errorf("DecodeOwnerName(%q) was accepted", name)
		}
	}
}
