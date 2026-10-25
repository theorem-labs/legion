package blame

import (
	"regexp"
	"strconv"
	"strings"
)

var diffHunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// EnrichFromDiff reads a member's unified diff (git diff -U0) and adds to the
// evidence what comparing declarations cannot see: edits inside a
// declaration's body, added and removed structure fields, and removed
// namespace lines. A change to a signature is not read here: IndexDeclarations
// finds it, because each declaration's Text is its whole signature.
//
// A hunk is evidence only for the declarations whose lines it changes. A
// declaration whose body line a hunk touches is edited only when its body
// code differs between the merge base and the head, so an unchanged
// neighbour is never edited: not by git's hunk header, which names the line
// above the hunk, and not by a hunk git placed across a line the neighbour
// repeats.
//
// beforeOwners and afterOwners hold ScanLineOwners of each changed file at the
// merge base and at the head.
func EnrichFromDiff(evidence *Evidence, diff string, beforeOwners, afterOwners map[string][]LineOwner) {
	var (
		oldFile, newFile string
		oldLine, newLine int
		oldLeft, newLeft int
		// touched holds the declarations whose body lines the diff changes.
		touched = make(map[DeclarationID]Declaration)
	)
	for line := range strings.SplitSeq(diff, "\n") {
		if oldLeft > 0 || newLeft > 0 {
			// The hunk's counts say which lines belong to it, so a removed
			// `-- comment` (`--- comment`) is not a file header.
			switch {
			case strings.HasPrefix(line, "-") && oldLeft > 0:
				changedLine(evidence, "-", beforeOwners[oldFile], oldFile, oldLine, touched)
				oldLine, oldLeft = oldLine+1, oldLeft-1
			case strings.HasPrefix(line, "+") && newLeft > 0:
				changedLine(evidence, "+", afterOwners[newFile], newFile, newLine, touched)
				newLine, newLeft = newLine+1, newLeft-1
			case strings.HasPrefix(line, " "):
				oldLine, oldLeft = oldLine+1, oldLeft-1
				newLine, newLeft = newLine+1, newLeft-1
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldFile, newFile = "", ""
		case strings.HasPrefix(line, "--- "):
			oldFile = diffPath(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			newFile = diffPath(strings.TrimPrefix(line, "+++ "), "b/")
		default:
			if match := diffHunkRE.FindStringSubmatch(line); match != nil {
				oldLine, oldLeft = atoi(match[1]), count(match[2])
				newLine, newLeft = atoi(match[3]), count(match[4])
			}
		}
	}
	beforeBodies, afterBodies := map[string]map[DeclarationID]string{}, map[string]map[DeclarationID]string{}
	for id, declaration := range touched {
		before, after := bodies(beforeBodies, beforeOwners, id.File), bodies(afterBodies, afterOwners, id.File)
		beforeBody, inBefore := before[id]
		afterBody, inAfter := after[id]
		if inBefore && inAfter && beforeBody != afterBody {
			evidence.Inside[id] = declaration
		}
	}
}

// bodies returns the body code of each declaration in a file, its body lines
// without comments joined with single spaces, reading the file's owners on
// first use. A declaration with no body line has an empty body.
func bodies(cache map[string]map[DeclarationID]string, owners map[string][]LineOwner, file string) map[DeclarationID]string {
	if found, ok := cache[file]; ok {
		return found
	}
	texts := map[DeclarationID][]byte{}
	for _, line := range owners[file] {
		if line.Owner.Name == "" {
			continue
		}
		id := line.Owner.ID()
		text := texts[id]
		if !line.IsSignature {
			text = appendFields(text, line.code)
		}
		texts[id] = text
	}
	out := make(map[DeclarationID]string, len(texts))
	for id, text := range texts {
		out[id] = string(text)
	}
	cache[file] = out
	return out
}

// changedLine records the evidence of one changed line, the 1-based line
// number of a file whose scan is owners, and adds the owner of a changed body
// line to touched.
func changedLine(evidence *Evidence, sign string, owners []LineOwner, file string, number int, touched map[DeclarationID]Declaration) {
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
	if match := fieldRE.FindStringSubmatch(line.code); match != nil && line.IsStructure {
		field := line.Owner.Name + "." + match[1]
		if sign == "+" {
			evidence.FieldsAdded[field] = struct{}{}
		} else {
			evidence.FieldsRemoved[field] = struct{}{}
		}
	}
	if !line.IsSignature {
		touched[line.Owner.ID()] = line.Owner
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
