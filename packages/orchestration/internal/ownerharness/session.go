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
	"regexp"
	"strings"
	"time"
)

var ownerCommand = regexp.MustCompile("Your owner command is: `([^`]+)`")

// Session is the scripted stand-in for an omp owner session: it follows owner.md step by step, with the owner
// command the prompt names. It subscribes through the harness listener at ENVOY_URL, records the subscription,
// works through its inbox, then records each delivery and works through its inbox again. It waits Delay before each
// claim and each answer, so that a kill can land before the claim, after it, or after the answer.
type Session struct {
	Command  []string
	Listener string
	Delay    time.Duration
	Log      io.Writer
}

// NewSession reads the owner command from the prompt file and the listener from ENVOY_URL.
// OWNER_HARNESS_STEP_DELAY, a Go duration, sets the delay (default 500ms).
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
	return Session{Command: strings.Fields(string(match[1])), Listener: listener, Delay: delay, Log: log}, nil
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

// Run follows owner.md until the listener closes the subscription or ctx ends.
func (s Session) Run(ctx context.Context) error {
	out, err := s.owner(ctx, "message-test", "topics")
	if err != nil {
		return err
	}
	topics := strings.Fields(out)
	query := url.Values{"topic": topics}
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
	seen := map[string]bool{}
	for lines.Scan() {
		var envelope Envelope
		if err := json.Unmarshal(lines.Bytes(), &envelope); err != nil {
			return fmt.Errorf("a delivery that is not an envelope: %q", lines.Text())
		}
		// A host drops a repeat of a dedupe key it has seen, as the Envoy plugin does.
		if seen[envelope.DedupeKey] {
			s.logf("dropped %s, a repeat of %s", envelope.EventID, envelope.DedupeKey)
			continue
		}
		seen[envelope.DedupeKey] = true
		topic := envelope.Topic
		var payload struct {
			Topic string `json:"topic"`
		}
		if json.Unmarshal([]byte(envelope.Payload), &payload) == nil && payload.Topic != "" {
			topic = payload.Topic
		}
		_, _ = s.owner(ctx, "message-test", "saw", envelope.EventID, topic)
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
