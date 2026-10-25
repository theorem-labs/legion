package graphcheck

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// stripComments returns text with every comment and every string or character literal blanked: each of their
// bytes other than a newline becomes a space, so lines and columns stay where they were.
func stripComments(text string) string {
	out := []byte(text)
	blank := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	n := len(text)
	for i := 0; i < n; {
		switch {
		case strings.HasPrefix(text[i:], "--"):
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				end = n - i
			}
			blank(i, i+end)
			i += end
		case strings.HasPrefix(text[i:], "/-"):
			depth, j := 1, i+2
			for j < n && depth > 0 {
				switch {
				case strings.HasPrefix(text[j:], "/-"):
					depth, j = depth+1, j+2
				case strings.HasPrefix(text[j:], "-/"):
					depth, j = depth-1, j+2
				default:
					j++
				}
			}
			blank(i, j)
			i = j
		case text[i] == '"' || (text[i] == 'r' && rawStringStart(text, i)):
			j := literalEnd(text, i)
			blank(i, j)
			i = j
		case text[i] == '\'' && charLiteral(text, i):
			j := i + 1
			if j < n && text[j] == '\\' {
				j += 2
			} else {
				_, size := utf8.DecodeRuneInString(text[j:])
				j += size
			}
			for j < n && text[j] != '\'' && text[j] != '\n' {
				j++
			}
			blank(i, min(j+1, n))
			i = j + 1
		default:
			i++
		}
	}
	return string(out)
}

// rawStringStart reports whether a raw string literal r"…" or r#"…"# starts at i.
func rawStringStart(text string, i int) bool {
	if i > 0 {
		if r, _ := utf8.DecodeLastRuneInString(text[:i]); identRune(r) {
			return false
		}
	}
	j := i + 1
	for j < len(text) && text[j] == '#' {
		j++
	}
	return j < len(text) && text[j] == '"'
}

// literalEnd is the end of the string literal that starts at i.
func literalEnd(text string, i int) int {
	n := len(text)
	if text[i] == 'r' {
		hashes := 0
		j := i + 1
		for j < n && text[j] == '#' {
			hashes, j = hashes+1, j+1
		}
		closing := "\"" + strings.Repeat("#", hashes)
		end := strings.Index(text[j+1:], closing)
		if end < 0 {
			return n
		}
		return j + 1 + end + len(closing)
	}
	for j := i + 1; j < n; j++ {
		switch text[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return n
}

// charLiteral reports whether the quote at i starts a character literal rather than ending an identifier (h').
func charLiteral(text string, i int) bool {
	if i > 0 {
		if r, _ := utf8.DecodeLastRuneInString(text[:i]); identRune(r) || r == '.' {
			return false
		}
	}
	rest := text[i+1:]
	if strings.HasPrefix(rest, "\\") {
		return strings.Contains(rest[:min(len(rest), 12)], "'")
	}
	_, size := utf8.DecodeRuneInString(rest)
	return size > 0 && len(rest) > size && rest[size] == '\''
}

func identRune(r rune) bool {
	return r == '_' || r == '\'' || r == '!' || r == '?' || unicode.IsLetter(r) || unicode.IsDigit(r) || r == '«' || r == '»' ||
		unicode.In(r, unicode.Greek, unicode.Letter) || (r >= 0x2080 && r <= 0x209C)
}

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

// lastComponent is a name's last dotted component.
func lastComponent(name string) string {
	return name[strings.LastIndexByte(name, '.')+1:]
}
