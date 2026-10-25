package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bars runs the timing driver's reducer over the runs it is given, one `label<TAB>name<TAB>seconds<TAB>status`
// for each line, and gives its exit code and its report.
func bars(t *testing.T, runs string) (int, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "runs.tsv")
	if err := os.WriteFile(file, []byte(runs), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "./timings.sh", "--bars", file)
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatalf("timings.sh --bars: %v\n%s", err, out)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

// lines are count runs of one command, each of seconds and status.
func lines(label string, count int, seconds, status string) string {
	var b strings.Builder
	for range count {
		b.WriteString(label + "\tX.y" + "\t" + seconds + "\t" + status + "\n")
	}
	return b.String()
}

// Every command of the acceptance bars is timed over the whole sample: the plan names what the sample holds, and
// a bar decided on the runs that happened to answer is not that measurement. A command that failed also returns
// fast, so counting it would let an unreachable index pass every bar.
func TestABarFailsWhenARunOfItsCommandDidNotAnswer(t *testing.T) {
	full := lines("where", 100, "0.010", "0") + lines("users", 100, "0.010", "0") +
		lines("states", 100, "0.010", "0") + lines("same", 100, "0.010", "0")
	if code, out := bars(t, full); code != 0 {
		t.Fatalf("a sample every command answered: exit %d\n%s", code, out)
	}
	// One answer and ninety-nine refusals of the index's database: the bar is not met, it is not measured.
	partial := strings.Replace(full, lines("users", 100, "0.010", "0"),
		lines("users", 1, "0.010", "0")+lines("users", 99, "4.000", "75"), 1)
	code, out := bars(t, partial)
	if code != 1 {
		t.Errorf("99 of 100 users runs failed: exit %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("the report says nothing about the runs that did not answer:\n%s", out)
	}
	if code, out := bars(t, strings.Replace(full, lines("users", 100, "0.010", "0"),
		lines("users", 100, "0.010", "75"), 1)); code != 1 {
		t.Errorf("no users run answered: exit %d, want 1\n%s", code, out)
	}
}

// land-hash, gate and ledger have no bar: the driver reports their times, and their own outcomes decide nothing
// about the bars. The gate answers a near match with 4 and a refusal with 1; the ledger answers a defect with 1.
func TestACommandWithNoBarDoesNotFailTheRun(t *testing.T) {
	runs := lines("where", 10, "0.010", "0") + lines("users", 10, "0.010", "0") +
		lines("states", 10, "0.010", "0") + lines("same", 10, "0.010", "0") +
		lines("land-hash", 10, "0.200", "0") + lines("gate", 10, "4.900", "4") + lines("ledger", 10, "0.700", "1")
	code, out := bars(t, runs)
	if code != 0 {
		t.Fatalf("a run whose gate answered a near match and whose ledger found a defect: exit %d\n%s", code, out)
	}
	for _, want := range []string{"summary\tgate", "summary\tledger", "summary\tland-hash"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report has no %q line:\n%s", want, out)
		}
	}
}

// A bar that no run measured is not a bar that was met: a run with no sample at all, or one whose --concepts
// file was empty so `states` never ran, measures nothing and fails.
func TestABarWithNoRunOfItsCommandFails(t *testing.T) {
	if code, out := bars(t, ""); code != 1 {
		t.Errorf("a measurement with no run at all: exit %d, want 1\n%s", code, out)
	}
	without := lines("where", 10, "0.010", "0") + lines("users", 10, "0.010", "0") + lines("same", 10, "0.010", "0")
	code, out := bars(t, without)
	if code != 1 {
		t.Errorf("a measurement in which states never ran: exit %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "states") {
		t.Errorf("the report does not name the command that no run measured:\n%s", out)
	}
}
