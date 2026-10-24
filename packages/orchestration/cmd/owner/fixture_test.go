package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

const (
	caller = "function:caller"
	second = "function:second@kernel/second.c"
	callee = "function:callee"
	state  = "struct:state"
	fact   = "fact-stack-alignment"

	needDraft  = "---\nauthor_model: openai-codex/gpt-5.6-terra\ncites: []\n---\n1. [C1] caller needs the state value to stay stable while it reads it. by: code kernel/caller.c:2\n"
	offerDraft = "---\nauthor_model: anthropic/claude-opus\ncomplete: false\nresolves: []\n---\n1. [C1] A reader of state.value sees the last value that a writer stored. by: code include/state.h:1\n"
)

type commandFixture struct {
	t           *testing.T
	ctx         context.Context
	dsn         string
	store       *ledger.Store
	root        string
	bare        string
	deploy      string
	listener    *httptest.Server
	mu          sync.Mutex
	posts       []noticePost
	refuse      bool   // the listener answers every publish with 503
	refuseTopic string // the listener answers every publish on this topic with 503
	refused     []noticePost
	env         map[string]string
}

type noticePost struct {
	Topic          string `json:"topic"`
	Message        string `json:"message"`
	Payload        string `json:"payload"`
	IdempotencyKey string `json:"idempotency_key"`
}

type noticePayload struct {
	Type     string         `json:"type"`
	Epoch    int            `json:"epoch"`
	From     string         `json:"from"`
	To       string         `json:"to"`
	Edge     string         `json:"edge"`
	Item     string         `json:"item"`
	Record   string         `json:"record"`
	Commit   string         `json:"commit"`
	Versions map[string]any `json:"versions"`
	Models   map[string]any `json:"models"`
}

func newCommandFixture(t *testing.T) *commandFixture {
	t.Helper()
	ctx := context.Background()
	dsn := testpg.Database(t, "owner_command")
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	f := &commandFixture{t: t, ctx: ctx, dsn: dsn, store: store, root: t.TempDir()}
	f.listener = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var post noticePost
		if r.URL.Path != "/v1/messages/publish" || json.NewDecoder(r.Body).Decode(&post) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.refuse || post.Topic == f.refuseTopic {
			f.refused = append(f.refused, post)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"listener unavailable"}`))
			return
		}
		f.posts = append(f.posts, post)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m-1"}`))
	}))
	t.Cleanup(f.listener.Close)

	testdata := filepath.Join("..", "..", "owner", "testdata")
	graphDir := filepath.Join(f.root, "graph")
	if err := os.MkdirAll(graphDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"graph.json", "owners.json", "topics.json", "facts.json"} {
		f.copyFile(filepath.Join(testdata, name), filepath.Join(graphDir, name))
	}
	ownerYAML := strings.NewReplacer("repo: example-repo", "repo: r", "active: skeleton/active/ns-match.yaml", "active: skeleton/active/fixture.yaml").
		Replace(string(f.read(filepath.Join(testdata, "owner.yaml"))))
	f.bare = f.createBareRepo(map[string]string{
		"skeleton/owner.yaml":           ownerYAML,
		"skeleton/PROTOCOL.yaml":        string(f.read(filepath.Join(testdata, "PROTOCOL.yaml"))),
		"skeleton/RULE.yaml":            string(f.read(filepath.Join(testdata, "RULE.yaml"))),
		"skeleton/facts.yaml":           string(f.read(filepath.Join(testdata, "facts.yaml"))),
		"skeleton/active/fixture.yaml":  "{name: fixture, nodes: [\"" + caller + "\", \"" + second + "\", \"" + state + "\", \"subsystem:cluster\"]}\n",
		"skeleton/active/ns-match.yaml": "{name: unused, nodes: []}\n",
	})
	dsnFile := filepath.Join(f.root, "ledger-dsn")
	f.writeFile(dsnFile, dsn)
	f.deploy = filepath.Join(f.root, "deploy.yaml")
	f.writeFile(f.deploy, strings.NewReplacer("/srv/owners/repo.git", f.bare, "/srv/owners/ledger-dsn", dsnFile, "/srv/graph/out", graphDir,
		"/srv/owners/state", f.stateDir()).Replace(string(f.read(filepath.Join(testdata, "deploy.yaml")))))
	f.env = map[string]string{
		"ENVOY_URL":   f.listener.URL,
		"ENVOY_TOKEN": "test-token",
		"HOME":        f.root,
	}
	return f
}

// stateDir is the deployment's state_dir, which holds undelivered.jsonl and the owners' worktrees.
func (f *commandFixture) stateDir() string { return filepath.Join(f.root, "state") }

// wt is owner's worktree, where the command commits its records.
func (f *commandFixture) wt(owner string) string {
	return filepath.Join(f.stateDir(), "owners", ownerpkg.Key(owner), "wt")
}

func (f *commandFixture) refuseNotices() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = true
}

type ownerRun struct {
	f     *commandFixture
	owner string
}

func (f *commandFixture) as(owner string) ownerRun { return ownerRun{f: f, owner: owner} }

func (o ownerRun) call(args ...string) (string, string, int) {
	return o.runContext(o.f.ctx, args...)
}

// runContext runs the command under ctx as --owner o.owner: cancelling it stops the command as a killed session
// stops its process.
func (o ownerRun) runContext(ctx context.Context, args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	code := run(ctx, append([]string{"--deploy", o.f.deploy, "--owner", o.owner}, args...), o.f.getenv, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

// getenv is the command's environment: the listener and HOME, and no owner variable.
func (f *commandFixture) getenv(key string) string { return f.env[key] }

func (o ownerRun) ok(args ...string) string {
	o.f.t.Helper()
	stdout, stderr, code := o.call(args...)
	if code != 0 {
		o.f.t.Fatalf("%s: owner %v exited %d: %s", o.owner, args, code, stderr)
	}
	return stdout
}

func (o ownerRun) fails(args ...string) (int, string) {
	o.f.t.Helper()
	_, stderr, code := o.call(args...)
	if code == 0 {
		o.f.t.Fatalf("%s: owner %v succeeded", o.owner, args)
	}
	return code, stderr
}

func (f *commandFixture) item(id string) ledger.Item {
	f.t.Helper()
	item, err := f.store.Item(f.ctx, "r", id)
	if err != nil {
		f.t.Fatalf("item %s: %v", id, err)
	}
	return item
}

func (f *commandFixture) enqueue(id, owner string) {
	f.t.Helper()
	if err := f.store.Enqueue(f.ctx, "r", id, owner); err != nil {
		f.t.Fatal(err)
	}
}

func (f *commandFixture) notices() []noticePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]noticePost(nil), f.posts...)
}

func (f *commandFixture) insertEvent(subject, data string) int64 {
	f.t.Helper()
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Close(f.ctx)
	var id int64
	if err := conn.QueryRow(f.ctx, `INSERT INTO events (repo, kind, subject, data) VALUES ('r', 'migration.round_failed', $1, $2) RETURNING id`, subject, []byte(data)).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *commandFixture) write(name, contents string) string {
	path := filepath.Join(f.root, "drafts", name)
	f.writeFile(path, contents)
	return path
}

func (f *commandFixture) writeFile(path, contents string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *commandFixture) read(path string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *commandFixture) copyFile(from, to string) { f.writeFile(to, string(f.read(from))) }

func (f *commandFixture) git(dir string, args ...string) string {
	f.t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *commandFixture) createBareRepo(files map[string]string) string {
	seed := filepath.Join(f.root, "seed")
	for path, contents := range files {
		f.writeFile(filepath.Join(seed, path), contents)
	}
	f.git(f.root, "init", "-q", "-b", "main", seed)
	f.git(seed, "add", ".")
	f.git(seed, "commit", "-q", "-m", "skeleton: fixture")
	bare := filepath.Join(f.root, "repo.git")
	f.git(f.root, "clone", "-q", "--bare", seed, bare)
	// The commands under test commit in worktrees of this repository with the process environment, so the
	// identity is the repository's own: a clean machine has no global one.
	f.git(f.root, "--git-dir="+bare, "config", "user.name", "Owner Test")
	f.git(f.root, "--git-dir="+bare, "config", "user.email", "owner-test@example.invalid")
	return bare
}

// writeScore commits slice/<main>.json with skeleton.node_instructions to the bare repository's score branch.
func (f *commandFixture) writeScore(counts map[string]int) {
	main := f.git(f.root, "--git-dir="+f.bare, "rev-parse", "main")
	data, err := json.Marshal(map[string]any{"skeleton": map[string]any{"node_instructions": counts}})
	if err != nil {
		f.t.Fatal(err)
	}
	scoreTree := filepath.Join(f.root, "score")
	f.git(f.root, "--git-dir="+f.bare, "worktree", "add", "-q", "--orphan", "-b", "score", scoreTree)
	f.writeFile(filepath.Join(scoreTree, "slice", main+".json"), string(data))
	f.git(scoreTree, "add", ".")
	f.git(scoreTree, "commit", "-q", "-m", "score: slice at "+main)
}

func (f *commandFixture) show(owner, path string) []byte {
	f.t.Helper()
	command := exec.Command("git", "--git-dir="+f.bare, "show", "owner/"+ownerpkg.Key(owner)+":"+path)
	out, err := command.Output()
	if err != nil {
		f.t.Fatalf("show %s on %s: %v", path, owner, err)
	}
	return out
}

func (f *commandFixture) lastCommit(owner, path string) string {
	return f.git(f.root, "--git-dir="+f.bare, "log", "-1", "--format=%H", "owner/"+ownerpkg.Key(owner), "--", path)
}

func (f *commandFixture) trailer(commit, key string) string {
	return f.git(f.root, "--git-dir="+f.bare, "log", "-1", "--format=%(trailers:key="+key+",valueonly)", commit)
}

func frontMatter(t *testing.T, data []byte) []byte {
	t.Helper()
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("record has no front matter: %q", text)
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		t.Fatalf("record front matter is not closed: %q", text)
	}
	return []byte(text[4 : 4+end+1])
}

func decodePayload(t *testing.T, post noticePost) noticePayload {
	t.Helper()
	var payload noticePayload
	if err := json.Unmarshal([]byte(post.Payload), &payload); err != nil {
		t.Fatalf("payload %q: %v", post.Payload, err)
	}
	return payload
}
