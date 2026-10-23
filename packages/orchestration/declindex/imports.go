package declindex

import "strings"

var importModifiers = map[string]bool{
	"all":     true,
	"meta":    true,
	"private": true,
	"public":  true,
}

// HeaderImports returns the module names of the header's import commands, in order.
func HeaderImports(src []byte) []string {
	var imports []string
	for _, line := range strings.Split(stripComments(string(src)), "\n") {
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}
		command := 0
		for command < len(words) && importModifiers[words[command]] {
			command++
		}
		if command == len(words) {
			continue
		}
		switch words[command] {
		case "prelude", "module":
			continue
		case "import":
			for _, module := range words[command+1:] {
				if !importModifiers[module] {
					imports = append(imports, module)
				}
			}
		default:
			return imports
		}
	}
	return imports
}
