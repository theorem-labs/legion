package messagetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The lines of an owner session's transcript as Oh My Pi 18.12 writes them, with pi-envoy's rendering of two of the
// driver's notices (TOON, as pi-envoy's renderInbound encodes the delivery) and its record of each delivery (the
// custom message's details: the subject it arrived on and its envelope's topic, event id and dedupe key), one
// delivery that is not the driver's, and the tool traffic between them.
const (
	titleLine  = `{"type":"title","v":1,"title":"Legion owner · MESSAGETEST","source":"user","updatedAt":"2026-10-24T04:53:15.000Z","pad":"          "}`
	headerLine = `{"type":"session","version":3,"id":"01a16c41-7fe7-70b4-9919-208d7325025a","timestamp":"2026-10-24T04:53:14.343Z","cwd":"/srv/state/owners/legion-mt-owner-struct_cmountpoint","title":"Legion owner","titleSource":"user"}`
	modelLine  = `{"type":"model_change","id":"62037d2d","parentId":null,"timestamp":"2026-10-24T04:53:14.504Z","runtime":{"version":"18.12.2"},"model":"openai/gpt-5.5","resolvedModelIsFallback":false}`
	toolLine   = `{"type":"message","id":"1c3312c3","parentId":"0fa27389","timestamp":"2026-10-24T04:53:44.100Z","message":{"role":"toolResult","content":[{"type":"text","text":"envoy-message is not a word the driver reads here"}]}}`
	arrivalA   = `{"type":"custom_message","customType":"envoy-message","content":"envoy:\n  from: agent\n  at: \"2026-10-24T04:53:43Z\"\n  id: cf83cf7322735fa058582509df1d646b\n  summary: \"message test arrival: a callers notice on notifications.example-message-test.mt.node.function:cleanup_mnt%40fs%2Fnamespace%2Ec.callers\"\n  message:\n    run: message-test-20261024t045309z-0a1b2c3d\n    step: arrival\n    topic: \"notifications.example-message-test.mt.node.function:cleanup_mnt%40fs%2Fnamespace%2Ec.callers\"\n    type: message-test","display":true,"details":{"subject":"notifications.example-message-test.mt.node.function:cleanup_mnt%40fs%2Fnamespace%2Ec.callers","topic":"notifications.example-message-test.mt.node.function:cleanup_mnt%40fs%2Fnamespace%2Ec.callers","event_id":"cf83cf7322735fa058582509df1d646b","dedupe_key":"publish.mt0a1b2c3d4e5f6a7b"},"attribution":"agent","id":"b3344aaf","parentId":"360ca9f4","timestamp":"2026-10-24T04:53:43.323Z","runtime":{"version":"18.12.2"}}`
	arrivalB   = `{"type":"custom_message","customType":"envoy-message","content":"envoy:\n  from: agent\n  at: \"2026-10-24T05:15:55Z\"\n  id: c3bb24243317a68bb9772a655b87c19f\n  summary: \"message test out of order: a needs notice on notifications.example-message-test.mt.node.struct:mountpoint.needs\"\n  message:\n    item: \"need:function:get_mountpoint@fs/namespace.c->struct:mountpoint@r2\"\n    run: message-test-20261024t045309z-0a1b2c3d\n    step: out of order\n    topic: notifications.example-message-test.mt.node.struct:mountpoint.needs\n    type: message-test","display":true,"details":{"subject":"notifications.example-message-test.mt.node.struct:mountpoint.needs","topic":"notifications.example-message-test.mt.node.struct:mountpoint.needs","event_id":"c3bb24243317a68bb9772a655b87c19f","dedupe_key":"publish.mt1b2c3d4e5f6a7b8c"},"attribution":"agent","id":"c03990ef","parentId":"718a60bf","timestamp":"2026-10-24T05:15:55.965Z","runtime":{"version":"18.12.2"}}`
	otherLine  = `{"type":"custom_message","customType":"envoy-message","content":"Unsubscribed from DEMO-1 by someone","display":true,"id":"d1","parentId":"c03990ef","timestamp":"2026-10-24T05:16:00.000Z"}`
	partial    = `{"type":"custom_message","customType":"envoy-message","content":"envoy:\n  id: still-being-wri`
)

func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The arrival of a notice is the custom_message entry that pi-envoy's delivery leaves in the session's transcript:
// its event id, the payload's run and step, the subject, topic and dedupe key of pi-envoy's record of the delivery,
// when the session recorded it and where. A delivery that is not the driver's notice is no failure, and a line still
// being written is read next time.
func TestReadArrivalsReadsTheDeliveriesAnOwnerTranscriptRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026-10-24T04-53-14-343Z_01a16c41.jsonl")
	writeTranscript(t, path, titleLine, headerLine, modelLine, arrivalA, toolLine, arrivalB, otherLine, partial)

	arrivals, err := readArrivals("struct:mountpoint", path)
	if err != nil {
		t.Fatal(err)
	}
	callers, needs := "notifications.example-message-test.mt.node.function:cleanup_mnt%40fs%2Fnamespace%2Ec.callers", "notifications.example-message-test.mt.node.struct:mountpoint.needs"
	want := []Arrival{
		{Owner: "struct:mountpoint", ID: "cf83cf7322735fa058582509df1d646b", Type: "message-test", Run: "message-test-20261024t045309z-0a1b2c3d",
			Step: "arrival", Topic: callers, Subject: callers, DedupeKey: "publish.mt0a1b2c3d4e5f6a7b",
			At: time.Date(2026, 10, 24, 4, 53, 43, 323e6, time.UTC), Transcript: path, Line: 4, Entry: "b3344aaf"},
		{Owner: "struct:mountpoint", ID: "c3bb24243317a68bb9772a655b87c19f", Type: "message-test", Run: "message-test-20261024t045309z-0a1b2c3d",
			Step: "out of order", Topic: needs, Subject: needs, DedupeKey: "publish.mt1b2c3d4e5f6a7b8c",
			At: time.Date(2026, 10, 24, 5, 15, 55, 965e6, time.UTC), Transcript: path, Line: 6, Entry: "c03990ef"},
	}
	if !slices.EqualFunc(arrivals, want, func(a, b Arrival) bool { return a == b }) {
		t.Fatalf("arrivals = %+v\nwant %+v", arrivals, want)
	}
}

// A delivery's topic is Envoy's record of it, which pi-envoy keeps in the transcript entry, and never the topic the
// driver wrote into the payload: a notice published on one topic and delivered on another must show the other.
func TestReadArrivalsTakesTheTopicFromEnvoysRecordNotThePayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	delivered := "notifications.example-message-test.mt.node.struct:mountpoint.users"
	moved := strings.Replace(arrivalB, `"subject":"notifications.example-message-test.mt.node.struct:mountpoint.needs","topic":"notifications.example-message-test.mt.node.struct:mountpoint.needs"`,
		`"subject":"`+delivered+`","topic":"`+delivered+`"`, 1)
	writeTranscript(t, path, headerLine, moved, "")
	arrivals, err := readArrivals("struct:mountpoint", path)
	if err != nil || len(arrivals) != 1 || arrivals[0].Topic != delivered || arrivals[0].Subject != delivered {
		t.Fatalf("readArrivals = %+v, %v; want the topic and subject %s that Envoy delivered it on, not the payload's", arrivals, err, delivered)
	}
}

// A driver's notice whose transcript entry holds no record of its topic (a pi-envoy that does not write one) cannot
// be judged, so the read stops, naming the line.
func TestReadArrivalsRefusesANoticeWithNoRecordOfItsTopic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	bare := regexp.MustCompile(`"details":\{[^}]*\},`).ReplaceAllString(arrivalB, "")
	writeTranscript(t, path, headerLine, bare, "")
	if _, err := readArrivals("struct:mountpoint", path); err == nil || !strings.Contains(err.Error(), path+":2") || !strings.Contains(err.Error(), "topic") {
		t.Fatalf("readArrivals = %v, want an error naming %s:2 and the missing topic", err, path)
	}
}

// A delivery of the driver's notice type that cannot be read stops the read: the run must not pass or fail a notice
// on a record it could not read.
func TestReadArrivalsRefusesANoticeItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	broken := `{"type":"custom_message","customType":"envoy-message","content":"envoy:\n  message:\n    type: message-test","timestamp":"2026-10-24T05:16:00.000Z"}`
	writeTranscript(t, path, headerLine, broken, "")
	if _, err := readArrivals("struct:mountpoint", path); err == nil || !strings.Contains(err.Error(), path+":2") {
		t.Fatalf("readArrivals = %v, want an error naming %s:2", err, path)
	}
}

// Oh My Pi keeps one working directory's sessions in one directory: every transcript of the owner's own sessions
// there, whatever relaunch made it, a fork (which names its parent session's id) included. A subagent's transcript
// names its parent's transcript file, and a file with no session header is no transcript.
func TestSessionTranscriptsListsTheSessionsOfOneOwner(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "2026-10-24T04-53-14-343Z_a.jsonl"), filepath.Join(dir, "2026-10-24T05-01-00-000Z_b.jsonl")
	fork := filepath.Join(dir, "2026-10-24T05-12-39-104Z_f.jsonl")
	writeTranscript(t, first, titleLine, headerLine, arrivalA, "")
	writeTranscript(t, second, strings.Replace(headerLine, "01a16c41-7fe7-70b4-9919-208d7325025a", "relaunched", 1), "")
	writeTranscript(t, fork, `{"type":"session","version":3,"id":"forked","cwd":"/srv","parentSession":"01a16c41-7fe7-70b4-9919-208d7325025a"}`, arrivalA, "")
	writeTranscript(t, filepath.Join(dir, "2026-10-24T05-02-00-000Z_c.jsonl"),
		`{"type":"session","version":3,"id":"sub","cwd":"/srv","parentSession":"`+first+`"}`, "")
	writeTranscript(t, filepath.Join(dir, "notes.jsonl"), `{"type":"note"}`, "")
	writeTranscript(t, filepath.Join(dir, "2026-10-24T04-53-14-343Z_a", "Helper.jsonl"), headerLine, "")

	got, err := sessionTranscripts(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Transcript{{Path: first, SessionID: "01a16c41-7fe7-70b4-9919-208d7325025a"}, {Path: second, SessionID: "relaunched"}, {Path: fork, SessionID: "forked"}}
	if !slices.Equal(got, want) {
		t.Fatalf("transcripts = %+v, want %+v", got, want)
	}

	// The fork's copy of its parent's delivery is that delivery, not a second one.
	d := &driver{nodes: []string{"struct:mountpoint"}, transcriptDirs: map[string][]string{"struct:mountpoint": {dir}}}
	arrivals, err := d.arrivals()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(arrivals["struct:mountpoint"]["cf83cf7322735fa058582509df1d646b"]); n != 1 {
		t.Fatalf("the delivery in a session and its fork counts %d times, want once", n)
	}
}

// A running session holds its transcript open, which is how the driver finds an owner pane's transcript.
func TestOpenTranscriptsFindsTheTranscriptAProcessHoldsOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	writeTranscript(t, path, titleLine, headerLine, "")
	other := filepath.Join(dir, "other.jsonl")
	writeTranscript(t, other, `{"type":"note"}`, "")
	holder := exec.Command("sh", "-c", `exec 3>>"$1" 4>>"$2"; exec sleep 60`, "sh", path, other)
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := openTranscripts(holder.Process.Pid)
		if slices.Equal(got, []Transcript{{Path: path, SessionID: "01a16c41-7fe7-70b4-9919-208d7325025a"}}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("openTranscripts = %+v, want only %s", got, path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
