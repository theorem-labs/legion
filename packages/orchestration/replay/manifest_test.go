package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadRefusesBadManifests(t *testing.T) {
	good := func() Manifest {
		return Manifest{Format: Format, Name: "r", Repository: "/repo", Base: strings.Repeat("a", 40),
			Members:     []Member{{ID: "m1", Head: strings.Repeat("b", 40)}, {ID: "m2", Head: strings.Repeat("c", 40)}},
			BuildRounds: []BuildRound{{Round: 1, Excluded: []string{"m2"}, Log: "build-round-1.log", RecordedKicks: []Kick{{Member: "m1", Reason: "ident:x"}}}}}
	}
	cases := map[string]func(*Manifest){
		"format":      func(m *Manifest) { m.Format = "x" },
		"format-v2":   func(m *Manifest) { m.Format = "landing-replay/v2" },
		"base":        func(m *Manifest) { m.Base = "abc" },
		"base-nonhex": func(m *Manifest) { m.Base = strings.Repeat("g", 40) },
		"head":        func(m *Manifest) { m.Members[0].Head = strings.Repeat("B", 40) },
		"head-nonhex": func(m *Manifest) { m.Members[0].Head = strings.Repeat("g", 40) },
		"duplicate":   func(m *Manifest) { m.Members[1].ID, m.BuildRounds[0].Excluded = "m1", []string{"m1"} },
		"excluded":    func(m *Manifest) { m.BuildRounds[0].Excluded = []string{"nobody"} },
		"kicked":      func(m *Manifest) { m.BuildRounds[0].RecordedKicks[0].Member = "nobody" },
		"log":         func(m *Manifest) { m.BuildRounds[0].Log = "missing.log" },
		"round-gaps":  func(m *Manifest) { m.BuildRounds[0].Round = 2 },
	}
	want := good()
	got, err := Load(writeManifest(t, want))
	if err != nil {
		t.Fatalf("good manifest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
	for name, change := range cases {
		m := good()
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
	manifest := minimalManifest("../outside.log")
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
	manifest.BuildRounds[0].Log = outside
	if _, err := Load(writeManifestIn(t, dir, manifest)); err == nil {
		t.Fatal("Load accepted an absolute build-round log outside the manifest directory")
	}
}

func TestLoadAcceptsConsecutiveRoundsAndRejectsAGap(t *testing.T) {
	dir := t.TempDir()
	manifest := minimalManifest("build-round-1.log")
	manifest.BuildRounds = append(manifest.BuildRounds, BuildRound{Round: 2, Log: "build-round-2.log"})
	for _, log := range []string{"build-round-1.log", "build-round-2.log"} {
		if err := os.WriteFile(filepath.Join(dir, log), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := writeManifestIn(t, dir, manifest)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load consecutive rounds: %v", err)
	}
	if !reflect.DeepEqual(got, manifest) {
		t.Fatalf("Load = %+v, want %+v", got, manifest)
	}
	manifest.BuildRounds[1].Round = 3
	if _, err := Load(writeManifestIn(t, dir, manifest)); err == nil {
		t.Fatal("Load accepted a gap between build rounds 1 and 3")
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
			manifest := minimalManifest(log)
			if _, err := Load(writeManifestIn(t, dir, manifest)); err == nil {
				t.Fatal("Load accepted a log that is not a regular file inside the manifest directory")
			}
		})
	}
}

func TestLoadRefusesTrailingJSON(t *testing.T) {
	manifest := minimalManifest("build-round-1.log")
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

func TestLoadRefusesUncleanLogPaths(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "logs")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build-round-1.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := minimalManifest("logs/../build-round-1.log")
	if _, err := Load(writeManifestIn(t, dir, manifest)); err == nil {
		t.Fatal("Load accepted an unclean path through a symbolic link")
	}
}

func TestLoadTreatsUnresolvedAsKnownMembers(t *testing.T) {
	manifest := minimalManifest("build-round-1.log")
	manifest.Unresolved = []Unresolved{{ID: "m2", Why: "missing head"}}
	manifest.BuildRounds[0].Excluded = []string{"m2"}
	manifest.BuildRounds[0].RecordedKicks = []Kick{{Member: "m2", Reason: "ident:x"}}
	if _, err := Load(writeManifest(t, manifest)); err != nil {
		t.Fatalf("Load rejected an exclusion and kick for an unresolved member: %v", err)
	}
}

func TestLoadRefusesDuplicateUnresolvedMembers(t *testing.T) {
	manifest := minimalManifest("build-round-1.log")
	manifest.Unresolved = []Unresolved{{ID: "m2", Why: "missing head"}, {ID: "m2", Why: "still missing"}}
	if _, err := Load(writeManifest(t, manifest)); err == nil {
		t.Fatal("Load accepted repeated unresolved member IDs")
	}
}

func TestLoadRefusesUnresolvedMemberAlreadyInMembers(t *testing.T) {
	manifest := minimalManifest("build-round-1.log")
	manifest.Unresolved = []Unresolved{{ID: "m1", Why: "missing head"}}
	if _, err := Load(writeManifest(t, manifest)); err == nil {
		t.Fatal("Load accepted an unresolved member already in Members")
	}
}

func TestLoadRequiresBuildRounds(t *testing.T) {
	common := `{"format":"` + Format + `","name":"r","repository":"/repo","base":"` + strings.Repeat("a", 40) +
		`","members":[{"id":"m1","head":"` + strings.Repeat("b", 40) + `"}],`
	cases := map[string]string{
		"missing":    common[:len(common)-1] + "}",
		"null":       common + `"buildRounds":null}`,
		"empty":      common + `"buildRounds":[]}`,
		"misspelled": common + `"buildRound":[{"round":1,"log":"build-round-1.log"}]}`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "build-round-1.log"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "manifest.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted a manifest without valid buildRounds")
			}
		})
	}
}

func TestLoadRefusesUnknownFields(t *testing.T) {
	manifest := minimalManifest("build-round-1.log")
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		json []byte
	}{
		{"top-level", append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"unexpected":true}`)...)},
		{"member", []byte(strings.Replace(string(data), `"id":"m1"`, `"id":"m1","unexpected":true`, 1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "build-round-1.log"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "manifest.json")
			if err := os.WriteFile(path, tc.json, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted an unknown JSON field")
			}
		})
	}
}

func TestLoadRefusesNoncanonicalLogPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build-round-1.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := minimalManifest("logs/../build-round-1.log")
	if _, err := Load(writeManifestIn(t, dir, manifest)); err == nil {
		t.Fatal("Load accepted a noncanonical log path")
	}
}

// A repeated key would let a later value silently reuse an earlier one: a second members array keeps the first
// array's head for a member that has none of its own.
func TestLoadRefusesRepeatedKeys(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	head := `{"format":"landing-replay/v1","base":"` + a + `",`
	rounds := `"buildRounds":[{"round":1,"log":"build-round-1.log"}]}`
	cases := map[string]string{
		"members twice":   head + `"members":[{"id":"m1","head":"` + b + `"}],"members":[{"id":"m2"}],` + rounds,
		"member id twice": head + `"members":[{"id":"m1","id":"m2","head":"` + b + `"}],` + rounds,
		"round key twice": head + `"members":[{"id":"m1","head":"` + b + `"}],"buildRounds":[{"round":1,"round":1,"log":"build-round-1.log"}]}`,
		"format twice":    `{"format":"x","format":"landing-replay/v1","base":"` + a + `","members":[],` + rounds,
	}
	for name, body := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "build-round-1.log"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "manifest.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if m, err := Load(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("%s: Load = %+v, %v; want a duplicate-key error", name, m, err)
		}
	}
}

// A key matches its field only when spelled exactly: "BASE" is not "base".
func TestLoadRefusesAKeyInAnotherCase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build-round-1.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"format":"landing-replay/v1","BASE":"` + strings.Repeat("a", 40) + `","members":[],"buildRounds":[{"round":1,"log":"build-round-1.log"}]}`
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err := Load(path); err == nil {
		t.Fatalf("Load = %+v; it read BASE as base", m)
	}
}

// A log is a regular file reached with no symbolic link, even one that stays inside the manifest's directory.
func TestLoadRefusesALogThroughASymbolicLinkInsideTheDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.log", filepath.Join(dir, "link.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("logs", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "r.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, log := range []string{"link.log", "alias/r.log"} {
		if m, err := Load(writeManifestIn(t, dir, minimalManifest(log))); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("log %q: Load = %+v, %v; want a symbolic-link refusal", log, m, err)
		}
	}
}

// The manifest and its logs are read from one directory: a manifest path through a symbolic link and `..` must not
// open the manifest in one directory and check its logs in another.
func TestLoadReadsTheManifestAndItsLogsInOneDirectory(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"actual/child", "lexical"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeManifestIn(t, filepath.Join(root, "actual"), minimalManifest("build.log")) // actual/build.log does not exist
	if err := os.WriteFile(filepath.Join(root, "lexical", "build.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "actual", "child"), filepath.Join(root, "lexical", "jump")); err != nil {
		t.Fatal(err)
	}
	if m, err := Load(root + "/lexical/jump/../manifest.json"); err == nil {
		t.Fatalf("Load = %+v; it read the manifest of actual/ and the log of lexical/", m)
	}
}

func minimalManifest(log string) Manifest {
	return Manifest{
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
