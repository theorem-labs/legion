package blame

import (
	"regexp"
	"strconv"
	"strings"
)

var diffHeaderRE = regexp.MustCompile(`^diff --git `)
var diffHunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// EnrichFromDiff parses a unified diff (git diff -U0 output) and fills
// Inside, FieldsAdded, FieldsRemoved and Namespaces in an existing Evidence.
// It also detects signature-continuation changes: when a line in the
// declaration's signature region (before := or where) changes, the
// declaration is added to Removed, Added and Lines so that EvidenceFor
// reports "signature changed".
//
// Hunk headers name the enclosing declaration. When a hunk changes lines
// inside a structure or class (so the line owners are nested declarations,
// not the structure itself), the structure gets Inside evidence from the
// hunk header context.
//
// beforeOwners and afterOwners map file paths to per-line ownership arrays
// produced by ScanLineOwners.
func EnrichFromDiff(evidence *Evidence, diff string, beforeOwners, afterOwners map[string][]LineOwner) {
	type hunkCandidate struct {
		id   DeclarationID
		decl Declaration
	}
	var (
		oldFile, newFile string
		oldLine, newLine int
		oldLeft, newLeft int
		hunkContext      *hunkCandidate
		hunkLineOwners   map[DeclarationID]struct{} // owners seen in the current hunk
	)

	flushHunk := func() {
		if hunkContext == nil {
			return
		}
		// Add Inside for the hunk header's declaration only when the hunk
		// contains lines owned by a different declaration (a nested
		// declaration inside the header's structure). When every changed
		// line is unowned (comment, blank, modifier) or is the same as the
		// header, Inside from the header would be noise.
		hasOtherOwner := false
		for id := range hunkLineOwners {
			if id != hunkContext.id {
				hasOtherOwner = true
				break
			}
		}
		if hasOtherOwner {
			evidence.Inside[hunkContext.id] = hunkContext.decl
		}
		hunkContext = nil
		hunkLineOwners = nil
	}

	for _, line := range strings.Split(diff, "\n") {
		if diffHeaderRE.MatchString(line) {
			flushHunk()
			oldFile, newFile = "", ""
			continue
		}
		if strings.HasPrefix(line, "--- ") {
			oldFile = diffPath(strings.TrimPrefix(line, "--- "), "a/")
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			newFile = diffPath(strings.TrimPrefix(line, "+++ "), "b/")
			continue
		}
		if match := diffHunkRE.FindStringSubmatch(line); match != nil {
			flushHunk()
			oldLine = atoi(match[1])
			oldLeft = max(atoi(match[2]), 1)
			if match[2] == "0" {
				oldLeft = 0
			}
			newLine = atoi(match[3])
			newLeft = max(atoi(match[4]), 1)
			if match[4] == "0" {
				newLeft = 0
			}
			hunkLineOwners = make(map[DeclarationID]struct{})
			// Parse the hunk context for the enclosing declaration.
			if context := strings.TrimSpace(match[5]); context != "" && newFile != "" {
				s := scanner{}
				result := s.scan(context)
				if result.kind == declarationLine {
					if owners, ok := afterOwners[newFile]; ok {
						for _, owner := range owners {
							if owner.Owner.Name != "" && Refers(result.declaration.Name, owner.Owner) {
								hunkContext = &hunkCandidate{id: owner.Owner.ID(), decl: owner.Owner}
								break
							}
						}
					}
				}
			}
			continue
		}
		if len(line) == 0 || (line[0] != '+' && line[0] != '-') {
			continue
		}
		sign := line[:1]
		body := line[1:]
		if sign == "-" {
			if oldLeft <= 0 {
				continue
			}
			processEvidenceLine(evidence, sign, body, oldFile, oldLine, beforeOwners, hunkLineOwners)
			oldLine++
			oldLeft--
		} else {
			if newLeft <= 0 {
				continue
			}
			processEvidenceLine(evidence, sign, body, newFile, newLine, afterOwners, hunkLineOwners)
			newLine++
			newLeft--
		}
	}
	flushHunk()
}

func processEvidenceLine(evidence *Evidence, sign, body, file string, lineNum int, owners map[string][]LineOwner, hunkOwners map[DeclarationID]struct{}) {
	ownerList, found := owners[file]
	if !found {
		return
	}
	// Scan the line to extract namespace and field information.
	s := scanner{}
	result := s.scan(body)
	// Removed namespace opening.
	if result.kind == namespaceLine && sign == "-" {
		namespace := Declaration{File: file, Name: result.namespace}
		evidence.Namespaces[namespace.ID()] = namespace
	}
	// Look up the owner from the pre-computed line scan.
	idx := lineNum - 1
	if idx < 0 || idx >= len(ownerList) {
		return
	}
	owner := ownerList[idx]
	if owner.Owner.Name == "" {
		return
	}
	// Record this owner so the hunk-header flush can skip it.
	if hunkOwners != nil {
		hunkOwners[owner.Owner.ID()] = struct{}{}
	}
	// Fields inside a structure or class.
	code := result.code
	if strings.TrimSpace(code) == "" {
		code = body
	}
	if match := fieldRE.FindStringSubmatch(code); match != nil && owner.IsStructure {
		field := owner.Owner.Name + "." + match[1]
		if sign == "+" {
			evidence.FieldsAdded[field] = struct{}{}
		} else {
			evidence.FieldsRemoved[field] = struct{}{}
		}
	}
	// Signature continuation change.
	if owner.IsSignature && result.kind != declarationLine {
		text := normaliseLine(body)
		if text != "" {
			id := owner.Owner.ID()
			evidence.Declarations[id] = owner.Owner
			evidence.Lines[id] = append(evidence.Lines[id], Line{Sign: sign, Text: text})
			if sign == "+" {
				evidence.Added[id] = owner.Owner
			} else {
				evidence.Removed[id] = owner.Owner
			}
		}
		return
	}
	// Body edit.
	if !owner.IsSignature && result.kind != declarationLine {
		evidence.Inside[owner.Owner.ID()] = owner.Owner
	}
}

func atoi(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}
