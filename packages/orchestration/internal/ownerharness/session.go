package ownerharness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var ownerCommand = regexp.MustCompile("Your owner command is: `([^`]+)`")

// Session is the scripted stand-in for an omp owner session: it follows owner.md step by step, with the owner
// command the prompt names. It subscribes through the harness listener at ENVOY_URL, records the subscription,
// works through its inbox, then records each of the driver's deliveries (never the other test owner's own notices)
// and works through its inbox again after every delivery. It waits Delay before each claim and each answer, so that a
// kill can land before the claim, after it, or after the answer.
//
// As omp does, it keeps a transcript of its session open while it runs, under Home's .omp/agent/sessions/ in a
// directory for its working directory, and appends each Envoy delivery it takes there as pi-envoy's envoy-message
// custom message. SkipSaw names a step whose notices it takes without running `owner message-test saw`, as an owner
// model may: the driver must judge those deliveries by the transcript.
type Session struct {
	Command  []string
	Listener string
	Delay    time.Duration
	Home     string
	SkipSaw  string
	Log      io.Writer
}

// NewSession reads the owner command from the prompt file, the listener from ENVOY_URL and the transcript root from
// HOME. OWNER_HARNESS_STEP_DELAY, a Go duration, sets the delay (default 500ms); OWNER_HARNESS_SKIP_SAW sets SkipSaw.
func NewSession(promptFile string, getenv func(string) string, log io.Writer) (Session, error) {
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		return Session{}, err
	}
	match := ownerCommand.FindSubmatch(prompt)
	if match == nil {
		return Session{}, fmt.Errorf("%s names no owner command", promptFile)
	}
	delay := 500 * time.Millisecond
	if text := getenv("OWNER_HARNESS_STEP_DELAY"); text != "" {
		if delay, err = time.ParseDuration(text); err != nil {
			return Session{}, fmt.Errorf("OWNER_HARNESS_STEP_DELAY: %w", err)
		}
	}
	listener := strings.TrimRight(getenv("ENVOY_URL"), "/")
	if listener == "" {
		return Session{}, fmt.Errorf("ENVOY_URL is unset")
	}
	home := getenv("HOME")
	if !filepath.IsAbs(home) {
		return Session{}, fmt.Errorf("HOME %q is not an absolute path: the session keeps its transcript under it", home)
	}
	return Session{Command: strings.Fields(string(match[1])), Listener: listener, Delay: delay, Home: home,
		SkipSaw: getenv("OWNER_HARNESS_SKIP_SAW"), Log: log}, nil
}

func (s Session) logf(format string, args ...any) {
	fmt.Fprintf(s.Log, "%s session %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

func (s Session) owner(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, s.Command[0], append(append([]string{}, s.Command[1:]...), args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	s.logf("owner %s: exit %v: %s %s", strings.Join(args, " "), err, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()))
	if err != nil {
		return stdout.String(), fmt.Errorf("owner %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// transcript is the session's open transcript.
type transcript struct {
	file *os.File
	id   string
}

// openTranscript makes the session's transcript as omp lays it out: <home>/.omp/agent/sessions/<working directory with
// each / as ->/<stamp>_<id>.jsonl, a title line and the session header, held open for the session's life.
func openTranscript(home, cwd string) (*transcript, error) {
	id := newID("session-")
	now := time.Now().UTC()
	dir := filepath.Join(home, ".omp", "agent", "sessions", strings.ReplaceAll(cwd, "/", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, strings.NewReplacer(":", "-", ".", "-").Replace(now.Format(time.RFC3339Nano))+"_"+id+".jsonl"),
		os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	t := &transcript{file: file, id: id}
	for _, entry := range []map[string]any{
		{"type": "title", "v": 1, "title": "owner harness session", "source": "user", "updatedAt": now},
		{"type": "session", "version": 3, "id": id, "timestamp": now, "cwd": cwd},
	} {
		if err := t.append(entry); err != nil {
			file.Close()
			return nil, err
		}
	}
	return t, nil
}

func (t *transcript) append(entry map[string]any) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = t.file.Write(append(line, '\n'))
	return err
}

// renderedEnvelope is the `envoy:` block pi-envoy renders for a delivery, in its field order.
type renderedEnvelope struct {
	From    string `yaml:"from"`
	At      string `yaml:"at"`
	ID      string `yaml:"id"`
	Summary string `yaml:"summary"`
	Message any    `yaml:"message,omitempty"`
}

// delivered appends a delivery to the transcript as the custom message of type envoy-message that pi-envoy hands the
// session: the rendered envelope, with the payload as message, and pi-envoy's record of the delivery as details (the
// harness listener delivers a notice on its own topic, so that is its subject).
func (t *transcript) delivered(envelope Envelope) error {
	var payload any = envelope.Payload
	var decoded map[string]any
	if json.Unmarshal([]byte(envelope.Payload), &decoded) == nil {
		payload = decoded
	}
	content, err := yaml.Marshal(map[string]renderedEnvelope{"envoy": {
		From: envelope.Source, At: time.UnixMilli(envelope.IssuedAt).UTC().Format(time.RFC3339), ID: envelope.EventID,
		Summary: envelope.PayloadSummary, Message: payload,
	}})
	if err != nil {
		return err
	}
	return t.append(map[string]any{
		"type": "custom_message", "customType": "envoy-message", "content": string(content), "display": true,
		"details": map[string]string{"subject": envelope.Topic, "topic": envelope.Topic, "event_id": envelope.EventID, "dedupe_key": envelope.DedupeKey},
		"id":      newID(""), "timestamp": time.Now().UTC(),
	})
}

// Run follows owner.md until the listener closes the subscription or ctx ends.
func (s Session) Run(ctx context.Context) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	session, err := openTranscript(s.Home, cwd)
	if err != nil {
		return fmt.Errorf("open the session transcript: %w", err)
	}
	defer session.file.Close()
	out, err := s.owner(ctx, "message-test", "topics")
	if err != nil {
		return err
	}
	topics := strings.Fields(out)
	query := url.Values{"topic": topics, "session": {session.id}, "dir": {cwd}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Listener+"/harness/v1/subscribe?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("subscribe: listener returned %s", response.Status)
	}
	lines := bufio.NewScanner(response.Body)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if !lines.Scan() {
		return fmt.Errorf("subscribe: no acknowledgement: %v", lines.Err())
	}
	s.logf("subscribed to %d topics", len(topics))
	if _, err := s.owner(ctx, "message-test", "subscribed"); err != nil {
		return err
	}
	s.inbox(ctx)
	seenEvents, seenKeys := map[string]bool{}, map[string]bool{}
	for lines.Scan() {
		var envelope Envelope
		if err := json.Unmarshal(lines.Bytes(), &envelope); err != nil {
			return fmt.Errorf("a delivery that is not an envelope: %q", lines.Text())
		}
		// A host drops a second copy of one event, and a repeat of a dedupe key that names its message (Envoy's
		// minted shape), as pi-envoy's claim does; a repeat of a key of any other shape is a new delivery to it.
		named := mintedDedupeKey.MatchString(envelope.DedupeKey)
		if seenEvents[envelope.EventID] || (named && seenKeys[envelope.DedupeKey]) {
			s.logf("dropped %s, a repeat of %s", envelope.EventID, envelope.DedupeKey)
			continue
		}
		seenEvents[envelope.EventID] = true
		if named {
			seenKeys[envelope.DedupeKey] = true
		}
		if err := session.delivered(envelope); err != nil {
			return fmt.Errorf("record the delivery %s in the transcript: %w", envelope.EventID, err)
		}
		topic := envelope.Topic
		var payload struct {
			Type  string `json:"type"`
			Topic string `json:"topic"`
			Step  string `json:"step"`
		}
		if json.Unmarshal([]byte(envelope.Payload), &payload) == nil && payload.Topic != "" {
			topic = payload.Topic
		}
		switch {
		case payload.Type != "message-test":
			s.logf("took %s, the other owner's own notice: owner.md records only the driver's", envelope.EventID)
		case s.SkipSaw != "" && payload.Step == s.SkipSaw:
			s.logf("took %s without recording it", envelope.EventID)
		default:
			_, _ = s.owner(ctx, "message-test", "saw", envelope.EventID, topic)
		}
		s.inbox(ctx)
	}
	return fmt.Errorf("the listener closed the subscription: %v", lines.Err())
}

// inbox claims and answers each item of the inbox until it lists none. An answer that fails ends this pass; the next
// delivery, or the next session, tries again.
func (s Session) inbox(ctx context.Context) {
	for {
		out, err := s.owner(ctx, "--json", "inbox")
		if err != nil {
			return
		}
		var items []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &items); err != nil || len(items) == 0 {
			return
		}
		for _, item := range items {
			time.Sleep(s.Delay)
			_, _ = s.owner(ctx, "claim", item.ID) // held by this owner already: an earlier session claimed it
			time.Sleep(s.Delay)
			if _, err := s.owner(ctx, "message-test", "answer", item.ID); err != nil {
				return
			}
		}
	}
}
