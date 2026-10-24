package messagetest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The driver judges a notice's arrival by the owner session's own record of it, never by the owner model recording
// it. pi-envoy hands each Envoy delivery it accepts to the session as an Oh My Pi custom message of type
// envoy-message (pi.sendMessage in packages/pi-envoy/extensions/envoy.ts), and Oh My Pi appends every message the
// session is given to its transcript: a JSONL file that the session's process holds open while it runs, where the
// delivery is a custom_message entry whose content is the rendered delivery, `envoy:` with the Envoy event `id` and
// the payload as `message`. pi-envoy's own list of deliveries (envoy_inbox) lives in the process's memory, and the
// listener does not deliver to a session that holds its own NATS connection, so neither keeps a record.
const envoyMessage = "envoy-message"

// noticeType is the payload type of every notice the driver publishes.
const noticeType = "message-test"

// Arrival is one Envoy delivery an owner session's transcript records: the event id, the payload's type, run, step
// and topic, when the session recorded it, where, and the transcript entry's own id.
type Arrival struct {
	Owner      string    `json:"owner"`
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Run        string    `json:"run"`
	Step       string    `json:"step"`
	Topic      string    `json:"topic"`
	At         time.Time `json:"at"`
	Transcript string    `json:"transcript"`
	Line       int       `json:"line"`
	Entry      string    `json:"entry"`
}

// transcriptLine is the part of one transcript entry the driver reads: ID is the entry's id, and on the session
// header the session's. A header's ParentSession names the session it came from: a subagent's names its parent's
// transcript file, and a fork's names its parent session's id. A fork starts with a copy of its parent's entries,
// each with the entry id it had there.
type transcriptLine struct {
	Type          string          `json:"type"`
	CustomType    string          `json:"customType"`
	Content       json.RawMessage `json:"content"`
	Timestamp     time.Time       `json:"timestamp"`
	ID            string          `json:"id"`
	ParentSession string          `json:"parentSession"`
}

// transcriptHeaderLines is how far into a transcript its session header can be: Oh My Pi writes a title line, then
// the header.
const transcriptHeaderLines = 8

// transcriptSession reads a file's session header: the session's id, and whether it is the owner's own session (one
// it started, or a fork of one) rather than a subagent's. A file with no header in its first lines is no transcript,
// and gives an empty id.
func transcriptSession(path string) (id string, own bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for range transcriptHeaderLines {
		data, err := reader.ReadBytes('\n')
		var line transcriptLine
		if json.Unmarshal(data, &line) == nil && line.Type == "session" {
			return line.ID, !strings.HasSuffix(line.ParentSession, ".jsonl"), nil
		}
		if err != nil {
			break
		}
	}
	return "", false, nil
}

// Transcript is an owner session's transcript: its file and the session's id.
type Transcript struct {
	Path      string `json:"path"`
	SessionID string `json:"session_id"`
}

// openTranscripts finds the transcripts that pid holds open: its open files that are Oh My Pi session transcripts
// of the owner's own sessions.
func openTranscripts(pid int) []Transcript {
	fds := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fds)
	if err != nil {
		return nil
	}
	var out []Transcript
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fds, entry.Name()))
		if err != nil || !strings.HasSuffix(target, ".jsonl") || slices.ContainsFunc(out, func(t Transcript) bool { return t.Path == target }) {
			continue
		}
		if id, own, err := transcriptSession(target); err == nil && id != "" && own {
			out = append(out, Transcript{Path: target, SessionID: id})
		}
	}
	return out
}

// sessionTranscripts lists the transcripts of the owner's own sessions in dir, the directory Oh My Pi keeps one
// working directory's sessions in, oldest name first. A subagent's transcript is in a directory of its own below it,
// and names its parent's transcript.
func sessionTranscripts(dir string) ([]Transcript, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	var out []Transcript
	for _, path := range paths {
		id, own, err := transcriptSession(path)
		if err != nil {
			return nil, err
		}
		if id != "" && own {
			out = append(out, Transcript{Path: path, SessionID: id})
		}
	}
	return out, nil
}

// renderedDelivery is the content of a delivery's custom message: the `envoy:` block pi-envoy renders, with the
// payload as message.
type renderedDelivery struct {
	Envoy struct {
		ID      string    `yaml:"id"`
		Message yaml.Node `yaml:"message"`
	} `yaml:"envoy"`
}

// noticePayload is the payload of the driver's notices.
type noticePayload struct {
	Type  string `yaml:"type"`
	Run   string `yaml:"run"`
	Step  string `yaml:"step"`
	Topic string `yaml:"topic"`
}

// readArrivals reads every delivery a transcript records, in order. A line still being written (no newline yet) is
// left for the next read. A delivery whose content cannot be read is an error when it names the driver's notice type:
// the run must not judge an arrival it cannot read; any other unreadable delivery is not the driver's and is skipped.
func readArrivals(owner, path string) ([]Arrival, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var arrivals []Arrival
	for number := 1; ; number++ {
		data, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return arrivals, nil
		}
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(data, []byte(envoyMessage)) {
			continue
		}
		var line transcriptLine
		if err := json.Unmarshal(data, &line); err != nil || line.Type != "custom_message" || line.CustomType != envoyMessage {
			continue
		}
		content, err := messageText(line.Content)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, number, err)
		}
		arrival, err := parseDelivery(content)
		if err != nil {
			if strings.Contains(content, noticeType) {
				return nil, fmt.Errorf("%s:%d: a delivery that names %s and cannot be read: %w", path, number, noticeType, err)
			}
			continue
		}
		arrival.Owner, arrival.At, arrival.Transcript, arrival.Line, arrival.Entry = owner, line.Timestamp, path, number, line.ID
		arrivals = append(arrivals, arrival)
	}
}

// messageText is a custom message's content as text: a string, or the text parts of a list of parts (a delivery that
// carries pictures).
func messageText(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("a custom message whose content is neither text nor parts: %w", err)
	}
	var out []string
	for _, part := range parts {
		if part.Type == "text" {
			out = append(out, part.Text)
		}
	}
	return strings.Join(out, "\n"), nil
}

// parseDelivery reads a rendered delivery: its event id, and the type, run, step and topic of a payload that is a
// mapping. A payload of any other shape is no notice of the driver's, and leaves them empty.
func parseDelivery(content string) (Arrival, error) {
	var rendered renderedDelivery
	if err := yaml.Unmarshal([]byte(content), &rendered); err != nil {
		return Arrival{}, err
	}
	if rendered.Envoy.ID == "" {
		return Arrival{}, errors.New("the delivery names no id")
	}
	arrival := Arrival{ID: rendered.Envoy.ID}
	if rendered.Envoy.Message.Kind == yaml.MappingNode {
		var payload noticePayload
		if err := rendered.Envoy.Message.Decode(&payload); err != nil {
			return Arrival{}, err
		}
		arrival.Type, arrival.Run, arrival.Step, arrival.Topic = payload.Type, payload.Run, payload.Step, payload.Topic
	}
	return arrival, nil
}
