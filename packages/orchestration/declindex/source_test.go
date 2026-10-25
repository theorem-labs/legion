package declindex

import "testing"

// definitionHash is the statement hash of a definition whose text is all of text.
func definitionHash(text string) string {
	return StatementHash(StatementText(text, 0, len(text), 0, "def"))
}

// White space inside a string or character literal is part of its value: a change to it changes the statement
// hash. White space between tokens, comments and doc strings are not part of the statement.
func TestStatementHashKeepsTheWhiteSpaceOfLiterals(t *testing.T) {
	base := definitionHash(`def message : String := "a b"`)
	for _, c := range []struct {
		name, text string
		same       bool
	}{
		{"two spaces in a string", `def message : String := "a  b"`, false},
		{"a line break in a string", "def message : String := \"a\nb\"", false},
		{"a tab in a string", "def message : String := \"a\tb\"", false},
		{"white space between tokens", "def  message :\n    String :=\n  \"a b\"", true},
		{"a comment", `def message : String := "a b" -- it's "one" value`, true},
		{"a doc string", "/-- The message: \"a  b\", isn't it? -/\ndef message : String := \"a b\"", true},
	} {
		if got := definitionHash(c.text) == base; got != c.same {
			t.Errorf("%s: hash equal to the base %v, want %v", c.name, got, c.same)
		}
	}
	space, tab := definitionHash(`def c : Char := ' '`), definitionHash("def c : Char := '\t'")
	if space == tab {
		t.Error("a space and a tab character literal have one statement hash")
	}
	for _, pair := range [][2]string{
		{`def message : String := r"a b"`, `def message : String := r"a  b"`},
		{`def message : String := r#"say "a b""#`, `def message : String := r#"say "a  b""#`},
	} {
		if definitionHash(pair[0]) == definitionHash(pair[1]) {
			t.Errorf("%s and %s have one statement hash", pair[0], pair[1])
		}
	}
	if primed := definitionHash("def f' (x' : Nat) : Nat := x' + 1"); primed != definitionHash("def f'  (x' : Nat)\n  : Nat := x'  +  1") {
		t.Error("the primes of names were read as character literals")
	}
	if image := definitionHash("def g := f '' s ∪ f '' t"); image != definitionHash("def g := f ''  s ∪\n  f '' t") {
		t.Error("the quotes of notation were read as character literals")
	}
}
