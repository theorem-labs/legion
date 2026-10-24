package tmux

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// A standing owner's pane, on a real tmux server, running the pinned Oh My Pi with this checkout's
// Envoy and Legion plugins: its first user turn is the start file its launch names
// (LEGION_OWNER_START_FILE), sent by the plugin after the registration, and the ledger command that
// file tells it to run reaches the pane's PATH with the launch's variables expanded, only after
// that turn began. The ledger is a stand-in on the pane's PATH that records its argv and answers
// `items` with no open item; the real ledger's `items` is proven against the ledger lane's build.
// The model gateway, the daemon's claim routes and the Envoy listener are one stand-in server, and
// the NATS address refuses every connection, so the Envoy plugin takes no delivery.
func TestRealTmuxStandingOwnersFirstTurnIsItsStartFile(t *testing.T) {
	omp := testbin.OMP(t)
	home := t.TempDir()
	testbin.OMPHome(t, omp, home)

	const (
		key        = "function:mnt_get_writers@fs/namespace.c"
		repository = "example/repository"
		template   = "You own one kernel function.\n"
	)
	scratch := t.TempDir()
	stubBin, dsnFile, argvFile := filepath.Join(scratch, "bin"), filepath.Join(scratch, "ledger-dsn"), filepath.Join(scratch, "ledger-argv")
	if err := os.MkdirAll(stubBin, 0o700); err != nil {
		t.Fatal(err)
	}
	ledgerStub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellprefix.Word(argvFile) + "\n" +
		"for arg in \"$@\"; do [ \"$arg\" = items ] && { echo '[]'; exit 0; }; done\necho '{}'\n"
	for path, content := range map[string]string{
		filepath.Join(stubBin, "ledger"): ledgerStub,
		dsnFile:                          "postgres://ledger@127.0.0.1:1/ledger\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	itemsCommand := `ledger --dsn-file "$LEGION_LEDGER_DSN_FILE" --json items ` + repository + ` --owner "$LEGION_OWNER"`

	standin := newOwnerStandin(t, argvFile, itemsCommand)
	writeOwnerProfile(t, home, standin.server.URL)
	nats := refusingListener(t)

	envoy, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "pi-envoy", "extensions", "envoy.ts"))
	if err != nil {
		t.Fatal(err)
	}
	legion, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "pi-legion", "extensions", "legion.ts"))
	if err != nil {
		t.Fatal(err)
	}
	invocation := shellprefix.Word(omp) + " --no-extensions -e " + shellprefix.Word(envoy) + " -e " + shellprefix.Word(legion) +
		" --no-skills --no-rules --no-lsp --no-title"
	r := newRig(t, func(o *Options) {
		o.DaemonURL, o.EnvoyURL, o.NatsURLs = standin.server.URL, standin.server.URL, []string{"nats://" + nats}
		o.Launchers = ompOnly(invocation)
		// The pane's Oh My Pi runs under the test's own HOME, whose default profile names the
		// stand-in gateway, never the operator's ~/.omp.
		environ := slices.DeleteFunc(slices.Clone(o.Environ), func(entry string) bool {
			name, _, _ := strings.Cut(entry, "=")
			return name == "HOME" || name == "OMP_PROFILE" || name == "PI_PROFILE"
		})
		o.Environ = append(environ, "HOME="+home)
	})

	spec := r.ownerSpec(key)
	startFile := filepath.Join(r.stateDir, "owners", string(spec.Claim), "start.md")
	start := prompts.OwnerStart(template, key, repository)
	if err := os.MkdirAll(filepath.Dir(startFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := podsafety.WriteReadOnly(startFile, []byte(start)); err != nil {
		t.Fatal(err)
	}
	spec.Env = map[string]string{
		"PATH":                     stubBin + ":/usr/local/bin:/usr/bin:/bin",
		"LEGION_OWNER":             key,
		"LEGION_LEDGER_REPOSITORY": repository,
		"LEGION_LEDGER_DSN_FILE":   dsnFile,
		"LEGION_OWNER_START_FILE":  startFile,
	}
	spec.Secrets = map[string]string{"ENVOY_TOKEN": "envoy-" + randomHex(t, 8)}
	standin.expect(spec.BootToken, spec.Claim)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	loc, err := r.rt.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn standing owner: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := r.tmux("capture-pane", "-p", "-S", "-200", "-t", loc.Tmux.Pane)
			t.Logf("the owner's pane:\n%s\nthe stand-in's requests: %s", out, strings.Join(standin.paths(), " "))
		}
	})

	eventually(t, 3*time.Minute, "the owner's second model turn, after its ledger call", func() bool { return len(standin.turnBodies()) >= 2 })
	turns := standin.turnBodies()

	messages, _ := turns[0]["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("the first turn carried no message: %v", turns[0])
	}
	// Oh My Pi opens a session's first user message with its own reminder of the date and the
	// working directory; everything after that block is the turn the plugin sent.
	first, _ := messages[0].(map[string]any)
	text := messageText(first)
	if strings.HasPrefix(text, "<system-reminder>") {
		if _, rest, ok := strings.Cut(text, "</system-reminder>\n"); ok {
			text = rest
		}
	}
	if role := first["role"]; role != "user" || text != start {
		t.Fatalf("the owner's first message is %v %q, want the user turn holding the start file:\n%s", role, text, start)
	}
	if standin.argvBeforeFirstTurn() {
		t.Fatal("the ledger ran before the owner's first turn reached the model")
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the stand-in ledger was never run: %v", err)
	}
	want := []string{"--dsn-file", dsnFile, "--json", "items", repository, "--owner", key}
	if got := strings.Split(strings.TrimRight(string(argv), "\n"), "\n"); !slices.Equal(got, want) {
		t.Fatalf("the ledger ran with %q, want %q", got, want)
	}
	// Oh My Pi's bash tool follows a command's output with its own wall-time line.
	if results := toolResultTexts(turns[1]); len(results) != 1 || !strings.HasPrefix(results[0], "[]\n") {
		t.Fatalf("the ledger's answer reached the model as %q, want one result starting with []", results)
	}
	if registered := standin.registeredBootTokens(); !slices.Equal(registered, []string{spec.BootToken}) {
		t.Fatalf("registrations = %q, want the owner's one boot token", registered)
	}
}

// ownerStandin is the one server a standing owner's pane talks to: the model gateway, which answers
// its first turn with a bash call of the ledger command and its later turns with text; the
// daemon's claim routes, which register the owner's boot token as its owner claim; and the Envoy
// listener, which accepts the session and the role claim and names it the role's holder.
type ownerStandin struct {
	server       *httptest.Server
	argvFile     string
	itemsCommand string

	mu                 sync.Mutex
	bootToken          string
	token              claim.Token
	turns              []map[string]any
	argvAtFirstTurn    bool
	registered         []string
	sessionID          string
	requestPaths       []string
	selfChecksAnswered int
}

func newOwnerStandin(t *testing.T, argvFile, itemsCommand string) *ownerStandin {
	t.Helper()
	s := &ownerStandin{argvFile: argvFile, itemsCommand: itemsCommand}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *ownerStandin) expect(bootToken string, token claim.Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bootToken, s.token = bootToken, token
}

func (s *ownerStandin) serve(w http.ResponseWriter, req *http.Request) {
	var body map[string]any
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&body)
	}
	s.mu.Lock()
	s.requestPaths = append(s.requestPaths, req.Method+" "+req.URL.Path)
	s.mu.Unlock()
	path := req.URL.Path
	switch {
	case path == "/anthropic/v1/messages":
		s.answerTurn(w, body)
	case strings.HasPrefix(path, "/anthropic/"):
		writeJSONBody(w, http.StatusOK, map[string]any{"data": []any{}})
	case path == "/legion/v1/claims/register":
		s.mu.Lock()
		boot, token := s.bootToken, s.token
		got, _ := body["bootToken"].(string)
		s.registered = append(s.registered, got)
		s.mu.Unlock()
		if got != boot {
			writeJSONBody(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
			return
		}
		writeJSONBody(w, http.StatusOK, claim.RegisterResponse{ClaimToken: token, Role: claim.RoleOwner, Generation: 1,
			Secret: "owner-secret", PromptAgents: []string{}})
	case path == "/legion/v1/claims/ready" || path == "/legion/v1/claims/exit":
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/legion/"):
		writeJSONBody(w, http.StatusNotFound, map[string]string{"error": "no stand-in route " + path})
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/v1/roles/"):
		s.mu.Lock()
		holder := s.sessionID
		s.mu.Unlock()
		role := strings.TrimPrefix(path, "/v1/roles/")
		if holder == "" {
			writeJSONBody(w, http.StatusNotFound, map[string]string{"error": "no holder for role " + role})
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"role": role, "holder": holder, "last_seen": 1})
	default:
		// The Envoy listener: the session's registration, the role claim, and any read, each
		// answered with the session's interest.
		session, _ := body["session_id"].(string)
		if session != "" {
			s.mu.Lock()
			s.sessionID = session
			s.mu.Unlock()
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"session_id": session, "machine_id": "owner-machine", "dir": "", "topics": []string{}})
	}
}

// answerTurn answers a Messages request: the run-end self-check, which pi-envoy asks inside a
// `<btw>` block and which is not a turn, with PROCEEDING; the first turn with one bash call of the
// ledger command, noting whether the ledger had already run; every later turn with text.
func (s *ownerStandin) answerTurn(w http.ResponseWriter, body map[string]any) {
	messages, _ := body["messages"].([]any)
	last, _ := json.Marshal(messages[len(messages)-1:])
	s.mu.Lock()
	var blocks []map[string]any
	id := ""
	if strings.Contains(string(last), "<btw>") {
		s.selfChecksAnswered++
		id = fmt.Sprintf("btw_%d", s.selfChecksAnswered)
		blocks = []map[string]any{{"type": "text", "text": "PROCEEDING"}}
	} else {
		s.turns = append(s.turns, body)
		id = fmt.Sprintf("msg_%d", len(s.turns))
		if len(s.turns) == 1 {
			_, err := os.Stat(s.argvFile)
			s.argvAtFirstTurn = err == nil
			blocks = []map[string]any{{"type": "tool_use", "name": "bash", "input": map[string]any{"command": s.itemsCommand}}}
		} else {
			blocks = []map[string]any{{"type": "text", "text": "No open item is left."}}
		}
	}
	s.mu.Unlock()
	w.Header().Set("content-type", "text/event-stream")
	_, _ = w.Write([]byte(messageStream(id, blocks)))
}

func (s *ownerStandin) turnBodies() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.turns)
}

func (s *ownerStandin) argvBeforeFirstTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.argvAtFirstTurn
}

func (s *ownerStandin) registeredBootTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.registered)
}

func (s *ownerStandin) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requestPaths)
}

// messageStream is the Anthropic Messages event stream for one scripted reply, as the plugins'
// real-Oh-My-Pi harness writes it (packages/pi-shared/test/omp-harness.ts, messageStream).
func messageStream(id string, blocks []map[string]any) string {
	var events [][2]any
	events = append(events, [2]any{"message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": "standin-model", "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 5, "output_tokens": 1},
	}}})
	stop := "end_turn"
	for index, block := range blocks {
		if block["type"] == "text" {
			events = append(events,
				[2]any{"content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}}},
				[2]any{"content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": block["text"]}}})
		} else {
			stop = "tool_use"
			input, _ := json.Marshal(block["input"])
			events = append(events,
				[2]any{"content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{
					"type": "tool_use", "id": fmt.Sprintf("toolu_%s_%d", id, index), "name": block["name"], "input": map[string]any{}}}},
				[2]any{"content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}}})
		}
		events = append(events, [2]any{"content_block_stop", map[string]any{"type": "content_block_stop", "index": index}})
	}
	events = append(events,
		[2]any{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}}},
		[2]any{"message_stop", map[string]any{"type": "message_stop"}})
	var out strings.Builder
	for _, event := range events {
		data, _ := json.Marshal(event[1])
		fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", event[0], data)
	}
	return out.String()
}

// writeOwnerProfile writes the default profile under home: the stand-in at base as its one
// provider, every model role on it, and every provider a devbox or runner could answer from
// without it disabled, as the plugins' harness does (writeStandinProfile).
func writeOwnerProfile(t *testing.T, home, base string) {
	t.Helper()
	agent := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	roles := ""
	for _, role := range []string{"default", "smol", "slow", "plan", "task", "commit", "tiny", "vision", "advisor"} {
		roles += "  " + role + ": standin/standin-model\n"
	}
	for name, content := range map[string]string{
		"models.yml": "providers:\n  standin:\n    baseUrl: " + base + "/anthropic\n    auth: apiKey\n    api: anthropic-messages\n" +
			"    apiKey: standin-key\n    models:\n      - id: standin-model\n        name: Stand-in\n        reasoning: false\n" +
			"        input: [text]\n        contextWindow: 200000\n        maxTokens: 8000\n" +
			"        cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0}\n",
		"config.yml": "enabledModels:\n  - standin/*\n" +
			"disabledProviders: [amazon-bedrock, bedrock-mantle, google, google-vertex, ollama, llama.cpp, lm-studio]\n" +
			"modelRoles:\n" + roles,
	} {
		if err := os.WriteFile(filepath.Join(agent, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// refusingListener is a TCP address that closes every connection as it opens: a NATS server the
// Envoy plugin connects to and never reaches.
func refusingListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return listener.Addr().String()
}

// messageText is a Messages message's text: its string content, or its text blocks joined.
func messageText(message map[string]any) string {
	switch content := message["content"].(type) {
	case string:
		return content
	case []any:
		var text strings.Builder
		for _, part := range content {
			if block, ok := part.(map[string]any); ok && block["type"] == "text" {
				s, _ := block["text"].(string)
				text.WriteString(s)
			}
		}
		return text.String()
	}
	return ""
}

// toolResultTexts is the text of every tool_result block in a Messages request's user messages.
func toolResultTexts(body map[string]any) []string {
	messages, _ := body["messages"].([]any)
	var texts []string
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] != "user" {
			continue
		}
		content, _ := message["content"].([]any)
		for _, part := range content {
			block, _ := part.(map[string]any)
			if block["type"] == "tool_result" {
				texts = append(texts, messageText(block))
			}
		}
	}
	return texts
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
