package blame

import (
	"regexp"
	"strconv"
	"strings"
)

var diffHunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// EnrichFromDiff reads a member's unified diff (git diff -U0) and adds to the
// evidence what comparing declarations cannot see: edits inside a
// declaration's body, added and removed structure fields, and removed
// namespace lines. A change to a signature is not read here: IndexDeclarations
// finds it, because each declaration's Text is its whole signature.
//
// A hunk header's context is the line git found above the hunk in the merge
// base. When that line is the declaration line of the nearest declaration
// above the hunk, and the hunk changes a line that another declaration owns,
// the hunk is also inside the header's declaration: a declaration added inside
// a structure edits the structure.
//
// beforeOwners and afterOwners hold ScanLineOwners of each changed file at the
// merge base and at the head.
func EnrichFromDiff(evidence *Evidence, diff string, beforeOwners, afterOwners map[string][]LineOwner) {
	var (
		oldFile, newFile string
		oldLine, newLine int
		oldLeft, newLeft int
		// header is the declaration the current hunk's header names, and
		// hunkOwners the owners of the lines the hunk changes.
		header     Declaration
		hunkOwners = make(map[DeclarationID]struct{})
	)
	flush := func() {
		if header.Name != "" {
			for id := range hunkOwners {
				if id != header.ID() {
					evidence.Inside[header.ID()] = header
					break
				}
			}
		}
		header = Declaration{}
		clear(hunkOwners)
	}
	for line := range strings.SplitSeq(diff, "\n") {
		if oldLeft > 0 || newLeft > 0 {
			// The hunk's counts say which lines belong to it, so a removed
			// `-- comment` (`--- comment`) is not a file header.
			switch {
			case strings.HasPrefix(line, "-") && oldLeft > 0:
				changedLine(evidence, "-", beforeOwners[oldFile], oldFile, oldLine, hunkOwners)
				oldLine, oldLeft = oldLine+1, oldLeft-1
			case strings.HasPrefix(line, "+") && newLeft > 0:
				changedLine(evidence, "+", afterOwners[newFile], newFile, newLine, hunkOwners)
				newLine, newLeft = newLine+1, newLeft-1
			case strings.HasPrefix(line, " "):
				oldLine, oldLeft = oldLine+1, oldLeft-1
				newLine, newLeft = newLine+1, newLeft-1
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			oldFile, newFile = "", ""
		case strings.HasPrefix(line, "--- "):
			oldFile = diffPath(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			newFile = diffPath(strings.TrimPrefix(line, "+++ "), "b/")
		default:
			match := diffHunkRE.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			flush()
			oldLine, oldLeft = atoi(match[1]), count(match[2])
			newLine, newLeft = atoi(match[3]), count(match[4])
			header = hunkDeclaration(beforeOwners[oldFile], oldLine, oldLeft, match[5])
		}
	}
	flush()
}

// hunkDeclaration returns the declaration that a hunk header's context names,
// or a zero Declaration. owners is the scan of the merge-base file, and start
// and length are the hunk's range in it.
//
// Git takes the context from the merge base: the nearest line above the hunk
// that starts with a letter, `_` or `$`, cut to 80 bytes. An added file has no
// merge-base side, so its hunk has no context. The context names a declaration
// only when it is the start of the declaration line of the nearest declaration
// above the hunk, so the header resolves by position, not by name.
func hunkDeclaration(owners []LineOwner, start, length int, context string) Declaration {
	context = normaliseLine(context)
	if context == "" {
		return Declaration{}
	}
	// above is the 1-based line above the hunk: a hunk that removes nothing
	// follows its start line.
	above := start - 1
	if length == 0 {
		above = start
	}
	for index := min(above, len(owners)) - 1; index >= 0; index-- {
		if owners[index].Kind == declarationLine {
			if strings.HasPrefix(normaliseLine(owners[index].text), context) {
				return owners[index].Owner
			}
			return Declaration{}
		}
	}
	return Declaration{}
}

// changedLine records the evidence of one changed line, the 1-based line
// number of a file whose scan is owners.
func changedLine(evidence *Evidence, sign string, owners []LineOwner, file string, number int, hunkOwners map[DeclarationID]struct{}) {
	index := number - 1
	if index < 0 || index >= len(owners) {
		return
	}
	line := owners[index]
	if line.Kind == namespaceLine && sign == "-" {
		namespace := Declaration{File: file, Name: line.namespace}
		evidence.Namespaces[namespace.ID()] = namespace
	}
	if line.Owner.Name == "" {
		return
	}
	hunkOwners[line.Owner.ID()] = struct{}{}
	if match := fieldRE.FindStringSubmatch(line.code); match != nil && line.IsStructure {
		field := line.Owner.Name + "." + match[1]
		if sign == "+" {
			evidence.FieldsAdded[field] = struct{}{}
		} else {
			evidence.FieldsRemoved[field] = struct{}{}
		}
	}
	if !line.IsSignature {
		evidence.Inside[line.Owner.ID()] = line.Owner
	}
}

// atoi parses a line number from a hunk header.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// count parses a line count from a hunk header, where an omitted count is 1.
func count(s string) int {
	if s == "" {
		return 1
	}
	return atoi(s)
}
