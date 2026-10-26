package conceptgate

import (
	"fmt"
	"regexp"
	"strings"
)

// A proposal is a Lean module of declarations. Compiling it must run no code of the proposal's: Lean runs `#eval`,
// `run_cmd` and their kind, `initialize` and `[init]` declarations, macros and elaborators the module defines,
// and `native_decide`, all as the gate's user. checkProposalSource refuses a module that contains any of them,
// outside comments and strings, and a `#` command where a command begins (commandHash). It reads the text, not the
// elaboration: code that the project itself defines (its own elaborators and tactics) runs as it does in the
// project's build.

// runningCommands are the commands and tactics that run code at compile time, refused unless used as a name in an
// ascription (`(prefix : Nat)`, a field `prefix : Nat`).
var runningCommands = []string{"run_cmd", "run_meta", "run_elab", "run_tac", "initialize", "builtin_initialize",
	"macro", "macro_rules", "elab", "elab_rules", "syntax", "declare_syntax_cat", "notation", "infix", "infixl",
	"infixr", "prefix", "postfix", "unsafe", "native_decide", "implemented_by", "extern"}

var (
	// hashCommand is a `#` command (`#eval`, `#print`, …) first on its line: a `#` word that no `[` follows, as one
	// follows the `#v` of a vector literal (`#v[…]`).
	hashCommand   = regexp.MustCompile(`^[ \t]*(#[A-Za-z_]\w*)(?:[^\w\[]|$)`)
	runningWord   = regexp.MustCompile(`(?:^|[^\w.'])(` + strings.Join(runningCommands, "|") + `)\b`)
	ascription    = regexp.MustCompile(`^\s+:`)
	attributeList = regexp.MustCompile(`(?:@\[|\battribute\s*\[)([^\]]*)\]`)
	runningAttr   = regexp.MustCompile(`\b(init|builtin_init|export|extern|implemented_by|command_elab|term_elab|tactic|macro|env_extension|builtin_\w+|register_\w+)\b`)
)

// leanCode drops comments (docstrings included) and the contents of string literals.
func leanCode(s string) string {
	var out strings.Builder
	n := len(s)
	for i := 0; i < n; {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				i = n
			} else {
				i += j
			}
		case strings.HasPrefix(s[i:], "/-"):
			d := 1
			i += 2
			for i < n && d > 0 {
				switch {
				case strings.HasPrefix(s[i:], "/-"):
					d++
					i += 2
				case strings.HasPrefix(s[i:], "-/"):
					d--
					i += 2
				default:
					i++
				}
			}
			out.WriteByte(' ')
		case s[i] == '"':
			i++
			for i < n && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			i++
			out.WriteString(`""`)
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

// commandHash gives the `#` command (`#eval`, `#print`, …) in code, or "": the first `#` word that begins a line
// outside every bracket. Lean begins a command only where the one before it ended, never inside an open bracket.
// Elsewhere a `#` word is a term or tactic token: an Iris proof-mode pattern (`iintro ⟨#Hinv, HR⟩`), also on a line
// of its own where a bracketed pattern goes on. This is a pass over text: it counts a bracket in a character
// literal (`'('`) too, and it takes for a command a bracketless pattern that goes on to a line of its own; Lean's
// parser allow-list (declindex.CheckSource) decides every command that it misses.
func commandHash(code string) string {
	depth := 0
	for _, line := range strings.SplitAfter(code, "\n") {
		if depth == 0 {
			if m := hashCommand.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
		for _, r := range line {
			switch r {
			case '(', '[', '{', '⟨', '⦃':
				depth++
			case ')', ']', '}', '⟩', '⦄':
				depth = max(depth-1, 0)
			}
		}
	}
	return ""
}

// checkProposalSource refuses a proposal that would run code while it is compiled.
func checkProposalSource(src string) error {
	code := leanCode(src)
	if m := commandHash(code); m != "" {
		return fmt.Errorf("the proposal holds a %q command: a proposal is declarations only", m)
	}
	for _, loc := range runningWord.FindAllStringSubmatchIndex(code, -1) {
		word := code[loc[2]:loc[3]]
		if ascription.MatchString(code[loc[3]:]) {
			continue
		}
		return fmt.Errorf("the proposal uses %q, which runs code at compile time: a proposal is declarations only", word)
	}
	for _, m := range attributeList.FindAllStringSubmatch(code, -1) {
		if a := runningAttr.FindString(m[1]); a != "" {
			return fmt.Errorf("the proposal uses the attribute %q, which runs or exports code: a proposal is declarations only", a)
		}
	}
	return nil
}
