package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// commandFixture is one host for the command: a ledger with repository r, an Envoy listener that records each publish,
// the librarian's bare repository with skeleton/owner.yaml at main, a DSN file and a deployment file.
type commandFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *ledger.Store
	root     string
	bare     string
	deploy   string
	dsn      string // the DSN file
	listener *httptest.Server
	mu       sync.Mutex
	posts    []noticePost
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
	f := &commandFixture{t: t, ctx: ctx, store: store, root: t.TempDir()}
	f.listener = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var post noticePost
		if r.URL.Path != "/v1/messages/publish" || json.NewDecoder(r.Body).Decode(&post) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.posts = append(f.posts, post)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m-1"}`))
	}))
	t.Cleanup(f.listener.Close)

	testdata := filepath.Join("..", "..", "owner", "testdata")
	ownerYAML := strings.Replace(string(f.read(filepath.Join(testdata, "owner.yaml"))), "repo: example-repo", "repo: r", 1)
	f.bare = f.createBareRepo(map[string]string{"skeleton/owner.yaml": ownerYAML})
	f.dsn = filepath.Join(f.root, "ledger-dsn")
	f.writeFile(f.dsn, dsn)
	f.deploy = filepath.Join(f.root, "deploy.yaml")
	f.writeFile(f.deploy, strings.NewReplacer("/srv/owners/repo.git", f.bare, "/srv/owners/ledger-dsn", f.dsn,
		"/srv/owners/state", f.stateDir()).Replace(string(f.read(filepath.Join(testdata, "deploy.yaml")))))
	return f
}

// stateDir is the deployment's state_dir, which holds undelivered.jsonl.
func (f *commandFixture) stateDir() string { return filepath.Join(f.root, "state") }

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

func isNotFound(err error) bool {
	var notFound *ledger.NotFoundError
	return errors.As(err, &notFound)
}

func (f *commandFixture) notices() []noticePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]noticePost(nil), f.posts...)
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
	// The command under test commits to this repository with the process environment, so the identity is the
	// repository's own: a clean machine has no global one.
	f.git(f.root, "--git-dir="+bare, "config", "user.name", "Owner Test")
	f.git(f.root, "--git-dir="+bare, "config", "user.email", "owner-test@example.invalid")
	return bare
}

func decodePayload(t *testing.T, post noticePost) noticePayload {
	t.Helper()
	var payload noticePayload
	if err := json.Unmarshal([]byte(post.Payload), &payload); err != nil {
		t.Fatalf("payload %q: %v", post.Payload, err)
	}
	return payload
}
