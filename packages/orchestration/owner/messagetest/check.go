package messagetest

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// The kinds of failure the report names. A step that did not finish is FailStep.
const (
	FailMissed          = "missed"
	FailDuplicate       = "duplicate"
	FailUnexpected      = "unexpected"
	FailNotCompleted    = "not-completed"
	FailCompletedTwice  = "completed-twice"
	FailMissingRecord   = "missing-record"
	FailDuplicateRecord = "duplicate-record"
	FailWrongResult     = "wrong-result"
	FailOutOfOrder      = "out-of-order"
	FailFollowUp        = "follow-up"
	FailLaunch          = "launch"
	FailSubscription    = "subscription"
	FailStep            = "step"
)

// Commit is one commit of an owner's branch: the owner whose branch holds it, and its trailers.
type Commit struct {
	Owner    string              `json:"owner"`
	SHA      string              `json:"sha"`
	Trailers map[string][]string `json:"trailers"`
}

// Recorded is the durable state a run left: the events of its ledger repository and the commits of each owner's
// branch, oldest first.
type Recorded struct {
	Events  []ledger.Event
	Commits []Commit
}

// ExpectedDelivery is one notice the driver published that one owner must record at most once. A Required notice
// must be recorded: the owner's session was running and subscribed when it was published. An optional one went to
// a session the driver kills, which can end before it records it; the ledger item is then the wake that lasts.
type ExpectedDelivery struct {
	Owner    string `json:"owner"`
	Topic    string `json:"topic"`
	ID       string `json:"id"`
	Required bool   `json:"required"`
}

// Outcome is how one work item must end. An item answered with a record completes exactly once, with the commit of
// its one record on Owner's branch, and enqueues FollowUp for FollowUpOwner exactly once. An item that a newer
// version superseded completes as superseded:<Superseded>, with no record. An Optional item (a follow-up nobody is
// asked to answer) may stay open, but completes at most once and as an answered item does.
type Outcome struct {
	Item          string `json:"item"`
	Owner         string `json:"owner"`
	Superseded    string `json:"superseded,omitempty"`
	FollowUp      string `json:"follow_up,omitempty"`
	FollowUpOwner string `json:"follow_up_owner,omitempty"`
	Optional      bool   `json:"optional,omitempty"`
}

// Expected is every delivery and item outcome a run must show.
type Expected struct {
	Deliveries []ExpectedDelivery `json:"deliveries"`
	Outcomes   []Outcome          `json:"outcomes"`
}

// Failure is one assertion the recorded state broke.
type Failure struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

// Check compares the recorded state of a run with what it must show. It names every missed, duplicate and
// unexpected delivery; every item completed twice or never, recorded twice or not at all, or completed with another
// result; every out-of-order item answered as if it were current; every follow-up enqueued twice, never or for
// another owner; and every record, completion or item of an item the run did not expect.
func Check(expected Expected, recorded Recorded) []Failure {
	var failures []Failure
	fail := func(kind, subject, format string, args ...any) {
		failures = append(failures, Failure{Kind: kind, Subject: subject, Detail: fmt.Sprintf(format, args...)})
	}
	checkDeliveries(expected.Deliveries, recorded.Commits, fail)

	completions, enqueues := map[string][]string{}, map[string][]string{}
	var completedOrder, enqueuedOrder []string
	for _, event := range recorded.Events {
		var data struct {
			Owner  string `json:"owner"`
			Result string `json:"result"`
		}
		_ = json.Unmarshal(event.Data, &data)
		switch event.Kind {
		case "item.completed":
			if _, seen := completions[event.Subject]; !seen {
				completedOrder = append(completedOrder, event.Subject)
			}
			completions[event.Subject] = append(completions[event.Subject], data.Result)
		case "item.enqueued":
			if _, seen := enqueues[event.Subject]; !seen {
				enqueuedOrder = append(enqueuedOrder, event.Subject)
			}
			enqueues[event.Subject] = append(enqueues[event.Subject], data.Owner)
		}
	}
	records := map[string][]Commit{}
	var recordedOrder []string
	for _, commit := range recorded.Commits {
		for _, item := range commit.Trailers[itemTrailer] {
			if _, seen := records[item]; !seen {
				recordedOrder = append(recordedOrder, item)
			}
			records[item] = append(records[item], commit)
		}
	}

	known := map[string]string{}
	for _, outcome := range expected.Outcomes {
		known[outcome.Item] = outcome.Owner
		if outcome.FollowUp != "" {
			if _, listed := known[outcome.FollowUp]; !listed {
				known[outcome.FollowUp] = outcome.FollowUpOwner
			}
		}
	}
	for _, outcome := range expected.Outcomes {
		checkOutcome(outcome, completions[outcome.Item], records[outcome.Item], enqueues, fail)
	}

	for _, item := range recordedOrder {
		owner, ok := known[item]
		for _, commit := range records[item] {
			if !ok || commit.Owner != owner {
				fail(FailUnexpected, item, "a record of an item the run did not expect of this owner, at %s on the branch of %s", commit.SHA, commit.Owner)
			}
		}
	}
	for _, item := range completedOrder {
		if _, ok := known[item]; !ok {
			fail(FailUnexpected, item, "a completion of an item the run did not expect, with results %v", completions[item])
		}
	}
	for _, item := range enqueuedOrder {
		if _, ok := known[item]; !ok {
			fail(FailUnexpected, item, "an item the run did not expect, enqueued for %v", enqueues[item])
		}
	}
	return failures
}

func checkDeliveries(deliveries []ExpectedDelivery, commits []Commit, fail func(kind, subject, format string, args ...any)) {
	type key struct{ owner, id string }
	type recording struct {
		topic, commit string
	}
	recorded := map[key][]recording{}
	var order []key
	for _, commit := range commits {
		ids, topics := commit.Trailers[DeliveryTrailer], commit.Trailers[TopicTrailer]
		if len(ids) == 0 {
			continue
		}
		topic := ""
		if len(topics) > 0 {
			topic = topics[0]
		}
		k := key{commit.Owner, ids[0]}
		if _, seen := recorded[k]; !seen {
			order = append(order, k)
		}
		recorded[k] = append(recorded[k], recording{topic: topic, commit: commit.SHA})
	}
	expected := map[key]string{}
	for _, delivery := range deliveries {
		k := key{delivery.Owner, delivery.ID}
		expected[k] = delivery.Topic
		if !delivery.Required || slices.ContainsFunc(recorded[k], func(r recording) bool { return r.topic == delivery.Topic }) {
			continue
		}
		fail(FailMissed, fmt.Sprintf("%s on %s for %s", delivery.ID, delivery.Topic, delivery.Owner),
			"the owner's session was running and subscribed when the driver published it, and it recorded no delivery of it")
	}
	for _, k := range order {
		topic, ok := expected[k]
		for _, r := range recorded[k] {
			if !ok || r.topic != topic {
				fail(FailUnexpected, fmt.Sprintf("%s on %s for %s", k.id, r.topic, k.owner),
					"recorded at %s; the driver published no such notice for this owner on this topic", r.commit)
			}
		}
		if n := len(recorded[k]); ok && n > 1 {
			fail(FailDuplicate, fmt.Sprintf("%s on %s for %s", k.id, topic, k.owner), "recorded %d times", n)
		}
	}
}

func checkOutcome(outcome Outcome, results []string, records []Commit, enqueues map[string][]string, fail func(kind, subject, format string, args ...any)) {
	item := outcome.Item
	own := slices.DeleteFunc(slices.Clone(records), func(commit Commit) bool { return commit.Owner != outcome.Owner })
	switch {
	case len(results) == 0 && !outcome.Optional:
		fail(FailNotCompleted, item, "never completed")
	case len(results) > 1:
		fail(FailCompletedTwice, item, "completed %d times, with results %v", len(results), results)
	case outcome.Superseded != "":
		want := "superseded:" + outcome.Superseded
		if len(own) > 0 || (len(results) == 1 && results[0] != want) {
			fail(FailOutOfOrder, item, "recorded at %v and completed as %v, want %s with no record", shas(own), results, want)
		}
	case len(own) > 1:
		fail(FailDuplicateRecord, item, "recorded %d times: %v", len(own), shas(own))
	case len(results) == 1 && len(own) == 0:
		fail(FailMissingRecord, item, "completed as %q with no record on the branch of %s", results[0], outcome.Owner)
	case len(results) == 1 && results[0] != own[0].SHA:
		fail(FailWrongResult, item, "completed as %q, want its record commit %s", results[0], own[0].SHA)
	}
	if outcome.FollowUp == "" {
		return
	}
	owners := enqueues[outcome.FollowUp]
	switch {
	case len(owners) == 0:
		fail(FailFollowUp, outcome.FollowUp, "the follow-up of %s was never enqueued", item)
	case len(owners) > 1:
		fail(FailFollowUp, outcome.FollowUp, "the follow-up of %s was enqueued %d times", item, len(owners))
	case owners[0] != outcome.FollowUpOwner:
		fail(FailFollowUp, outcome.FollowUp, "the follow-up of %s was enqueued for %s, want %s", item, owners[0], outcome.FollowUpOwner)
	}
}

func shas(commits []Commit) []string {
	out := make([]string, 0, len(commits))
	for _, commit := range commits {
		out = append(out, commit.SHA)
	}
	return out
}
