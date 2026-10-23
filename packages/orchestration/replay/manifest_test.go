package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRefusesBadManifests(t *testing.T) {
	good := Manifest{Format: Format, Name: "r", Repository: "/repo", Base: strings.Repeat("a", 40),
		Members:     []Member{{ID: "m1", Head: strings.Repeat("b", 40)}, {ID: "m2", Head: strings.Repeat("c", 40)}},
		BuildRounds: []BuildRound{{Round: 1, Excluded: []string{"m2"}, Log: "build-round-1.log", RecordedKicks: []Kick{{Member: "m1", Reason: "ident:x"}}}}}
	cases := map[string]func(*Manifest){
		"format":     func(m *Manifest) { m.Format = "x" },
		"base":       func(m *Manifest) { m.Base = "abc" },
		"head":       func(m *Manifest) { m.Members[0].Head = strings.Repeat("B", 40) },
		"duplicate":  func(m *Manifest) { m.Members[1].ID = "m1" },
		"excluded":   func(m *Manifest) { m.BuildRounds[0].Excluded = []string{"nobody"} },
		"kicked":     func(m *Manifest) { m.BuildRounds[0].RecordedKicks[0].Member = "nobody" },
		"log":        func(m *Manifest) { m.BuildRounds[0].Log = "missing.log" },
		"round-gaps": func(m *Manifest) { m.BuildRounds[0].Round = 2 },
	}
	if _, err := Load(writeManifest(t, good)); err != nil {
		t.Fatalf("good manifest: %v", err)
	}
	for name, change := range cases {
		m := good
		m.Members = append([]Member(nil), good.Members...)
		m.BuildRounds = []BuildRound{good.BuildRounds[0]}
		m.BuildRounds[0].Excluded = append([]string(nil), good.BuildRounds[0].Excluded...)
		m.BuildRounds[0].RecordedKicks = append([]Kick(nil), good.BuildRounds[0].RecordedKicks...)
		change(&m)
		if _, err := Load(writeManifest(t, m)); err == nil {
			t.Fatalf("%s: Load accepted it", name)
		}
	}
}

func TestLoadRefusesALogOutsideTheManifestDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside.log")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Format:     Format,
		Name:       "r",
		Repository: "/repo",
		Base:       strings.Repeat("a", 40),
		Members:    []Member{{ID: "m1", Head: strings.Repeat("b", 40)}},
		BuildRounds: []BuildRound{{
			Round: 1,
			Log:   "../outside.log",
		}},
	}
	path := filepath.Join(dir, "manifest.json")
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a build-round log outside the manifest directory")
	}
}

func TestLoadRefusesLogSymlinksAndNonRegularFiles(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string) string
	}{
		{
			name: "file symlink",
			setup: func(t *testing.T, dir string) string {
				outside := filepath.Join(filepath.Dir(dir), "outside.log")
				if err := os.WriteFile(outside, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "link.log")); err != nil {
					t.Fatal(err)
				}
				return "link.log"
			},
		},
		{
			name: "parent symlink",
			setup: func(t *testing.T, dir string) string {
				outside := filepath.Join(filepath.Dir(dir), "outside")
				if err := os.Mkdir(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(outside, "build.log"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "logs")); err != nil {
					t.Fatal(err)
				}
				return "logs/build.log"
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, dir string) string {
				if err := os.Mkdir(filepath.Join(dir, "build.log"), 0o700); err != nil {
					t.Fatal(err)
				}
				return "build.log"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := tc.setup(t, dir)
			manifest := Manifest{
				Format:     Format,
				Name:       "r",
				Repository: "/repo",
				Base:       strings.Repeat("a", 40),
				Members:    []Member{{ID: "m1", Head: strings.Repeat("b", 40)}},
				BuildRounds: []BuildRound{{
					Round: 1,
					Log:   log,
				}},
			}
			if _, err := Load(writeManifestIn(t, dir, manifest)); err == nil {
				t.Fatal("Load accepted a log that is not a regular file inside the manifest directory")
			}
		})
	}
}

func TestLoadRefusesTrailingJSON(t *testing.T) {
	manifest := Manifest{
		Format:     Format,
		Name:       "r",
		Repository: "/repo",
		Base:       strings.Repeat("a", 40),
		Members:    []Member{{ID: "m1", Head: strings.Repeat("b", 40)}},
		BuildRounds: []BuildRound{{
			Round: 1,
			Log:   "build-round-1.log",
		}},
	}
	path := writeManifest(t, manifest)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n{}"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a manifest followed by another JSON value")
	}
}

func writeManifest(t *testing.T, manifest Manifest) string {
	t.Helper()
	dir := t.TempDir()
	path := writeManifestIn(t, dir, manifest)
	if err := os.WriteFile(filepath.Join(dir, "build-round-1.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeManifestIn(t *testing.T, dir string, manifest Manifest) string {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
