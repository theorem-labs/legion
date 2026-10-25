package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// readyIDs lists the migrations that ready-list prints, in its order.
func (cli batchCLI) readyIDs() []string {
	cli.t.Helper()
	var records []struct {
		ID string `json:"id"`
	}
	cli.decode(cli.invoke("--json", "ready-list", "r"), &records)
	ids := []string{}
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

func declineFields(id string, version int64, successor string, returns ...map[string]any) map[string]any {
	if returns == nil {
		returns = []map[string]any{}
	}
	return map[string]any{
		"repo": "r", "id": id, "reason": "queue_removed", "returns": returns, "successor": successor,
		"expected_version": version, "record": cliRecord("declined", "successor", successor),
	}
}

func cliReturn(id string) map[string]any {
	return map[string]any{"repo": "r", "id": id, "generation": 1, "reason": "build failed", "batch": "batch-1", "key": "batch-1/" + id}
}

// decline-batch with one member returned prints the declined batch at its new version with the successor; ready-list
// then holds the other member, batches --active lists the declined batch until its successor exists, and the other
// member is admitted under the successor's id. A retry prints the stored batch, and a different decline from the old
// version is refused with stale_batch. After the decline, a takeover of a member succeeds.
func TestDeclineBatchCommandReturnsMembersAndNamesTheSuccessor(t *testing.T) {
	cli := newBatchCLI(t)
	kept := cli.openReady("M-1", "alice", "a.lean", nil)
	cli.openReady("M-2", "bob", "b.lean", nil)
	blamed := cli.ready("M-2", "b.lean", nil)
	cli.create("batch-1", cliRecord("checked"))
	admitted := cli.admit(admitFields("batch-1", 1, kept, blamed))

	request := declineFields("batch-1", admitted.RecordVersion, "batch-2", cliReturn("M-2"))
	var declined cliLandedBatch
	cli.decode(cli.invoke("decline-batch", "--request-file", cli.request(request)), &declined)
	if declined.Phase != "declined" || declined.Admission != "declined" || declined.Successor != "batch-2" ||
		declined.RecordVersion != admitted.RecordVersion+1 || len(declined.DeclineDigest) != 64 {
		t.Fatalf("decline-batch printed %+v", declined)
	}
	if ready := cli.readyIDs(); !slices.Equal(ready, []string{"M-1"}) {
		t.Fatalf("ready-list after the decline = %q", ready)
	}
	if members := cli.members("batch-1"); !slices.Equal(members, []string{"M-1:false", "M-2:false"}) {
		t.Fatalf("members after the decline = %q", members)
	}
	if active := cli.active(); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("batches --active = %q, want the declined batch until its successor exists", active)
	}

	events := len(cli.events(""))
	retry := declineFields("batch-1", admitted.RecordVersion, "batch-2", cliReturn("M-2"))
	retry["record"] = cliRecord("other")
	var replayed cliLandedBatch
	cli.decode(cli.invoke("decline-batch", "--request-file", cli.request(retry)), &replayed)
	if !reflect.DeepEqual(replayed, declined) {
		t.Fatalf("retry printed %+v, want the stored %+v", replayed, declined)
	}
	cli.refused("stale_batch", "decline-batch", "--request-file", cli.request(declineFields("batch-1", admitted.RecordVersion, "batch-2")))
	cli.assertUnchanged(declined.cliBatch, events)

	cli.create("batch-2", cliRecord("checked"))
	if successor := cli.admit(admitFields("batch-2", 1, kept)); successor.Admission != "admitted" {
		t.Fatalf("the successor printed %+v", successor)
	}
	if active := cli.active(); !slices.Equal(active, []string{"batch-2"}) {
		t.Fatalf("batches --active with the successor = %q", active)
	}
	cli.invoke("takeover", "r", "M-2", "zoe", "1")
}

// A malformed decline-batch exits 2 and changes nothing.
func TestDeclineBatchCommandRefusesMalformedInput(t *testing.T) {
	cli, admitted := admittedCLIBatch(t)
	events := len(cli.events(""))
	fields := declineFields("batch-1", admitted.RecordVersion, "")
	fields["merged_commit"] = "m"
	path := cli.request(fields)
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"decline-batch", "r", "batch-1"}, "decline-batch --request-file"},
		{[]string{"decline-batch", "--request-file", path}, "unknown field"},
	} {
		if code, stdout, stderr := cli.call(test.args...); code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 2 naming %q", test.args, code, stdout, stderr, test.stderr)
		}
	}
	cli.assertUnchanged(admitted, events)
}
