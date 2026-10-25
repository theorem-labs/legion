package graphcheck_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conf", "graphcheck.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = "index: declindex.yaml\nrepository: ../repo\nroots: [Lib]\nbuild_roots: [Lib]\n"

func TestLoadConfigResolvesPathsAgainstItsDirectory(t *testing.T) {
	path := writeConfig(t, minimalConfig+"root_new_modules: true\n")
	cfg, err := graphcheck.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if cfg.Index != filepath.Join(dir, "declindex.yaml") || cfg.Repository != filepath.Join(filepath.Dir(dir), "repo") {
		t.Fatalf("index %q, repository %q", cfg.Index, cfg.Repository)
	}
	if !cfg.RootNewModules || len(cfg.Roots) != 1 || cfg.BuildRoots[0] != "Lib" {
		t.Fatalf("config %+v", cfg)
	}
}

func TestLoadConfigRefusesAnUnknownKeyNamingIt(t *testing.T) {
	for _, body := range []string{minimalConfig + "edges: {}\n", minimalConfig + "links: {link_file: []}\n"} {
		_, err := graphcheck.LoadConfig(writeConfig(t, body))
		if err == nil || !(strings.Contains(err.Error(), "edges") || strings.Contains(err.Error(), "link_file")) {
			t.Errorf("LoadConfig(%q) = %v, want an error naming the key", body, err)
		}
	}
}

func TestLoadConfigRefusesAMissingRequiredKey(t *testing.T) {
	for key, body := range map[string]string{
		"index":       "repository: r\nroots: [Lib]\nbuild_roots: [Lib]\n",
		"repository":  "index: i\nroots: [Lib]\nbuild_roots: [Lib]\n",
		"roots":       "index: i\nrepository: r\nbuild_roots: [Lib]\n",
		"build_roots": "index: i\nrepository: r\nroots: [Lib]\n",
	} {
		if _, err := graphcheck.LoadConfig(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("without %s: LoadConfig = %v, want an error naming it", key, err)
		}
	}
}

func TestLoadConfigRefusesAMalformedPatternNamingItsKey(t *testing.T) {
	for key, extra := range map[string]string{
		"links.contract_name":             "links: {link_files: ['Lib/*.lean'], contract_name: '(['}\n",
		"links.link_files":                "links: {link_files: ['Lib/[.lean'], contract_name: 'X'}\n",
		"links.exclude_files":             "links: {link_files: ['Lib/*.lean'], contract_name: 'X', exclude_files: ['Lib/[']}\n",
		"links.open_marker_min_reason":    "links: {link_files: ['Lib/*.lean'], contract_name: 'X', open_marker: 'ask:', open_marker_min_reason: -1}\n",
		"links.contract_name is required": "links: {link_files: ['Lib/*.lean']}\n",
		"producers.interface_names":       "producers: {interface_names: ['(['], binder_files: ['Lib/*.lean']}\n",
		"producers.binder_files":          "producers: {interface_names: ['X'], binder_files: ['Lib/[']}\n",
		"build_roots":                     "index: i\nrepository: r\nroots: [Lib]\nbuild_roots: [Other]\n",
	} {
		body := minimalConfig + extra
		if key == "build_roots" {
			body = extra
		}
		if _, err := graphcheck.LoadConfig(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), strings.Fields(key)[0]) {
			t.Errorf("%s: LoadConfig = %v, want an error naming it", key, err)
		}
	}
}

func TestGlobsAreRootRelativeWithWholeComponentDoubleStars(t *testing.T) {
	cfg, err := graphcheck.LoadConfig(writeConfig(t, minimalConfig+"links: {link_files: ['Lib/**/Link*.lean'], contract_name: 'X', exclude_files: ['Lib/*/Skip.lean']}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"Lib/LinkA.lean":          true, // ** matches zero components
		"Lib/A/B/LinkC.lean":      true,
		"Lib/A/NoLink.lean":       false,
		"Other/Lib/LinkA.lean":    false, // root-relative, not a suffix
		"Lib/A/LinkA.lean/x":      false,
		"Lib/A/Link/Other.lean":   false, // * stays within one component
		"Lib/A/Skip.lean":         false,
		"Lib/A/B/Skip/LinkX.lean": true,
	} {
		if got := cfg.Links.IsLinkFile(path); got != want {
			t.Errorf("IsLinkFile(%q) = %v, want %v", path, got, want)
		}
	}
	if !cfg.Links.Excluded("Lib/A/Skip.lean") || cfg.Links.Excluded("Lib/A/B/Skip.lean") {
		t.Error("exclude_files: * must match exactly one component")
	}
}

func TestContractNameMustMatchTheWholeHead(t *testing.T) {
	cfg, err := graphcheck.LoadConfig(writeConfig(t, minimalConfig+"links: {link_files: ['Lib/**/Link*.lean'], contract_name: '_*[A-Z][A-Z0-9_]{3,}'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for head, want := range map[string]bool{"SIZE_OK": true, "__ABCD": true, "SIZE_OKx": false, "Lib.SIZE_OK": false, "ABC": false} {
		if got := cfg.Links.IsContract(head); got != want {
			t.Errorf("IsContract(%q) = %v, want %v", head, got, want)
		}
	}
}
