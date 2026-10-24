package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

type itemsCLI struct {
	t   *testing.T
	dsn string
}

func newItemsCLI(t *testing.T) itemsCLI {
	t.Helper()
	cli := itemsCLI{t: t, dsn: testpg.Database(t, "ledger_items")}
	cli.invoke("repo", "ensure", "r", "3")
	return cli
}

func (cli itemsCLI) call(args ...string) (int, string, string) {
	cli.t.Helper()
	var out, err bytes.Buffer
	code := run(context.Background(), args, func(key string) string {
		if key == "LEDGER_DSN" {
			return cli.dsn
		}
		return ""
	}, &out, &err)
	return code, out.String(), err.String()
}

func (cli itemsCLI) invoke(args ...string) string {
	cli.t.Helper()
	code, stdout, stderr := cli.call(args...)
	if code != 0 {
		cli.t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	return stdout
}

// openItem is one element of `ledger items`, with the JSON the command printed for it. A field the command leaves out
// stays absent from fields.
type openItem struct {
	ID         string     `json:"id"`
	Owner      string     `json:"owner"`
	State      string     `json:"state"`
	Holder     string     `json:"holder"`
	Generation int64      `json:"generation"`
	ExpiresAt  *time.Time `json:"expires_at"`
	raw        json.RawMessage
	fields     map[string]json.RawMessage
}

// items reads `items r --owner owner`, which prints the same JSON with or without --json, and returns its elements
// and the whole output.
func (cli itemsCLI) items(owner string) ([]openItem, string) {
	cli.t.Helper()
	output := cli.invoke("--json", "items", "r", "--owner", owner)
	if plain := cli.invoke("items", "r", "--owner", owner); plain != output {
		cli.t.Fatalf("items without --json printed %q, want the JSON %q", plain, output)
	}
	var elements []json.RawMessage
	if err := json.Unmarshal([]byte(output), &elements); err != nil || elements == nil {
		cli.t.Fatalf("items of %s = %q, want a JSON list: %v", owner, output, err)
	}
	items := make([]openItem, len(elements))
	for i, element := range elements {
		if err := json.Unmarshal(element, &items[i]); err != nil {
			cli.t.Fatal(err)
		}
		if err := json.Unmarshal(element, &items[i].fields); err != nil {
			cli.t.Fatal(err)
		}
		items[i].raw = element
	}
	return items, output
}

// items lists one owner's open items in enqueue order, never another owner's or a completed one: a free item with
// no holder or lease end, a supervised lease with its holder and generation and no lease end, and a fixed-end lease
// with its end. An owner with no items gets an empty list.
func TestItemsCommandListsOneOwnersOpenItemsInEnqueueOrder(t *testing.T) {
	cli := newItemsCLI(t)
	const owner, other, holder = "struct:mount", "function:get_mountpoint", "session-1"
	for _, item := range [][2]string{{"c", owner}, {"x", other}, {"a", owner}, {"y", other}, {"b", owner}, {"d", owner}} {
		cli.invoke("enqueue", "r", item[0], item[1])
	}
	cli.invoke("claim", "r", "a", holder, "0")
	cli.invoke("claim", "r", "b", holder, "1h")
	cli.invoke("claim", "r", "d", holder, "0")
	cli.invoke("complete", "r", "d", holder, "1", "proved")
	var leased struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(cli.invoke("--json", "item", "r", "b")), &leased); err != nil || leased.ExpiresAt.IsZero() {
		t.Fatalf("fixed-end lease: %v %+v", err, leased)
	}

	got, output := cli.items(owner)
	if len(got) != 3 || got[0].ID != "c" || got[1].ID != "a" || got[2].ID != "b" {
		t.Fatalf("items of %s = %s, want c, a and b in enqueue order", owner, output)
	}
	for _, item := range got {
		if item.Owner != owner || item.State != "open" {
			t.Fatalf("item %s = %s, want an open item of %s", item.ID, item.raw, owner)
		}
		if _, ok := item.fields["result"]; ok {
			t.Fatalf("open item %s prints a result: %s", item.ID, item.raw)
		}
	}
	free, supervised, fixed := got[0], got[1], got[2]
	if _, ok := free.fields["holder"]; ok || free.Generation != 0 {
		t.Fatalf("free item = %s, want no holder at generation 0", free.raw)
	}
	if _, ok := free.fields["expires_at"]; ok {
		t.Fatalf("free item prints a lease end: %s", free.raw)
	}
	if supervised.Holder != holder || supervised.Generation != 1 {
		t.Fatalf("supervised lease = %s, want holder %s at generation 1", supervised.raw, holder)
	}
	if _, ok := supervised.fields["expires_at"]; ok {
		t.Fatalf("supervised lease prints a lease end: %s", supervised.raw)
	}
	if fixed.Holder != holder || fixed.Generation != 1 || fixed.ExpiresAt == nil || !fixed.ExpiresAt.Equal(leased.ExpiresAt) {
		t.Fatalf("fixed-end lease = %s, want holder %s at generation 1 until %s", fixed.raw, holder, leased.ExpiresAt)
	}

	if got, output := cli.items(other); len(got) != 2 || got[0].ID != "x" || got[1].ID != "y" {
		t.Fatalf("items of %s = %s, want x and y", other, output)
	}
	if got, output := cli.items("global:nobody"); len(got) != 0 || output != "[]\n" {
		t.Fatalf("items of an owner with no items = %q, want an empty list", output)
	}
}

// A malformed items exits 2 and names the problem: no --owner, a flag other than --owner, a missing or extra
// argument, or an empty owner. A repository that does not exist exits 3.
func TestItemsCommandUsageErrors(t *testing.T) {
	cli := newItemsCLI(t)
	cli.invoke("enqueue", "r", "item-1", "struct:mount")
	const usage = "items <repo> --owner <key>"
	for _, test := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"no arguments", []string{"items"}, usage},
		{"no owner", []string{"items", "r"}, usage},
		{"owner without its flag", []string{"items", "r", "struct:mount"}, usage},
		{"flag without a value", []string{"items", "r", "--owner"}, usage},
		{"another flag", []string{"items", "r", "--holder", "struct:mount"}, usage},
		{"flag before the repository", []string{"items", "--owner", "struct:mount", "r"}, usage},
		{"extra argument", []string{"items", "r", "--owner", "struct:mount", "extra"}, usage},
		{"empty owner", []string{"items", "r", "--owner", ""}, "empty owner"},
		{"blank owner", []string{"items", "r", "--owner", " "}, "empty owner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, args := range [][]string{test.args, append([]string{"--json"}, test.args...)} {
				code, stdout, stderr := cli.call(args...)
				if code != 2 || stdout != "" || !strings.Contains(stderr, test.stderr) {
					t.Fatalf("%q: code=%d stdout=%q stderr=%q, want exit 2 naming %q", args, code, stdout, stderr, test.stderr)
				}
			}
		})
	}
	if code, stdout, stderr := cli.call("items", "missing", "--owner", "struct:mount"); code != 3 {
		t.Fatalf("items of a missing repository: code=%d stdout=%q stderr=%q, want 3", code, stdout, stderr)
	}
}
