package owner

import (
	"strconv"
	"strings"
)

// Paths holds the templates from skeleton/owner.yaml.
type Paths struct {
	EdgeModuleTemplate       string `yaml:"edge_module"`
	EdgeTheoremTemplate      string `yaml:"edge_theorem"`
	InterfaceModuleTemplate  string `yaml:"interface_module"`
	InterfaceTheoremTemplate string `yaml:"interface_theorem"`
	NodeModuleTemplate       string `yaml:"node_module"`
	EdgeRecordTemplate       string `yaml:"edge_record"`
	AgreementIDTemplate      string `yaml:"agreement_id"`
}

// EdgeModule names an edge-proof source module.
func (p Paths) EdgeModule(source, target string) string {
	return renderPath(p.EdgeModuleTemplate, pathValues{source: source, target: target})
}

// EdgeTheorem names an edge-proof theorem.
func (p Paths) EdgeTheorem(source, target string) string {
	return renderPath(p.EdgeTheoremTemplate, pathValues{source: source, target: target})
}

// InterfaceModule names an interface-proof source module.
func (p Paths) InterfaceModule(dataStructure, iface string) string {
	return renderPath(p.InterfaceModuleTemplate, pathValues{dataStructure: dataStructure, iface: iface})
}

// InterfaceTheorem names an interface-proof theorem.
func (p Paths) InterfaceTheorem(dataStructure, iface string) string {
	return renderPath(p.InterfaceTheoremTemplate, pathValues{dataStructure: dataStructure, iface: iface})
}

// NodeModule names an owner's node module.
func (p Paths) NodeModule(id string) string {
	return renderPath(p.NodeModuleTemplate, pathValues{node: id})
}

// EdgeRecord names an edge record.
func (p Paths) EdgeRecord(source, target string) string {
	return renderPath(p.EdgeRecordTemplate, pathValues{source: source, target: target})
}

// AgreementID names an agreement record.
func (p Paths) AgreementID(source, target string, revision int) string {
	return renderPath(p.AgreementIDTemplate, pathValues{source: source, target: target, revision: revision})
}

type pathValues struct {
	node, source, target string
	dataStructure, iface string
	revision             int
}

func renderPath(template string, values pathValues) string {
	var out strings.Builder
	out.Grow(len(template) + len(values.source) + len(values.target))
	for rest := template; ; {
		start := strings.IndexByte(rest, '{')
		if start < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:start])
		rest = rest[start+1:]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			out.WriteByte('{')
			out.WriteString(rest)
			return out.String()
		}
		switch rest[:end] {
		case "key":
			out.WriteString(Key(values.node))
		case "key_source":
			out.WriteString(Key(values.source))
		case "hex16_target":
			out.WriteString(Hex16(values.target))
		case "key_ds":
			out.WriteString(Key(values.dataStructure))
		case "hex16_interface":
			out.WriteString(Hex16(values.iface))
		case "source":
			out.WriteString(values.source)
		case "target":
			out.WriteString(values.target)
		case "n":
			out.WriteString(strconv.Itoa(values.revision))
		default:
			out.WriteByte('{')
			out.WriteString(rest[:end])
			out.WriteByte('}')
		}
		rest = rest[end+1:]
	}
}
