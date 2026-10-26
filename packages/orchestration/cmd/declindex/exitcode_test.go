package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// cutProxy forwards one Postgres connection after another to the server that dsn names, and gives the connection
// string through it and a cut that ends every connection and refuses the next one, as a server that stops or a
// network that fails does.
func cutProxy(t *testing.T, dsn string) (proxyDSN string, cut func()) {
	t.Helper()
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host := target.Host
	if !strings.Contains(host, ":") {
		host += ":5432"
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var open []net.Conn
	done := false
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", host)
			if err != nil {
				_ = client.Close()
				return
			}
			mu.Lock()
			if done {
				mu.Unlock()
				_, _ = client.Close(), server.Close()
				return
			}
			open = append(open, client, server)
			mu.Unlock()
			go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
			go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
		}
	}()
	through := *target
	through.Host = listener.Addr().String()
	t.Cleanup(func() { _ = listener.Close() })
	return through.String(), func() {
		mu.Lock()
		done = true
		conns := open
		open = nil
		mu.Unlock()
		_ = listener.Close()
		for _, c := range conns {
			_ = c.Close()
		}
	}
}

// cutAfter is the writer a command's messages go through: it cuts the index's database when a message holds mark,
// and passes every message on.
type cutAfter struct {
	w    io.Writer
	mark string
	cut  func()
	once sync.Once
}

func (c *cutAfter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if strings.Contains(string(p), c.mark) {
		c.once.Do(c.cut)
	}
	return n, err
}

// A refresh that commits and then loses its database has written the index: every module is there, and the
// command must ask the landing program to run it again (exit 75), not report what it checked as a defect (exit
// 3), which the landing program escalates instead. The count of modules that the command reads after the commit
// is a round trip of its own (P-11).
func TestARefreshThatLosesItsDatabaseAfterItCommitsAsksForARetry(t *testing.T) {
	config := fixture(t)
	raw, err := os.ReadFile(indexDSNFile(config))
	if err != nil {
		t.Fatal(err)
	}
	proxyDSN, cut := cutProxy(t, strings.TrimSpace(string(raw)))
	dsnFile := filepath.Join(filepath.Dir(config), "proxy.dsn")
	writeTestFile(t, dsnFile, proxyDSN)
	var out bytes.Buffer
	// The last message of a refresh comes after both of its write transactions commit, and before the command
	// counts the modules.
	errOut := &cutAfter{w: &bytes.Buffer{}, mark: "re-read", cut: cut}
	code := run(context.Background(), []string{"--config", config, "--index-dsn-file", dsnFile, "refresh"},
		func(string) string { return "" }, &out, errOut)
	if code != 75 {
		t.Fatalf("a refresh whose database went away after it committed: exit %d, want 75\n%s", code,
			errOut.w.(*bytes.Buffer).String())
	}
}

// Every command that opens the index without a schema name or a connection string is a usage error (exit 2): a
// configuration is corrected once, where exit 3 tells the gate's owner that a later refresh will answer, so the
// proposal would be tried again for ever.
func TestTheGateWithoutAConnectionStringIsAUsageError(t *testing.T) {
	config := fixture(t)
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [Golden, Sub]\nregistry: registry\n")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(config), "tree", "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	proposal := filepath.Join(filepath.Dir(config), "proposal.json")
	writeTestFile(t, proposal, `{"id":"p1","concept":{"name":"Gold.Probe","home":"Golden.lean","meaning":"A probe."},`+
		`"source":"def Gold.Probe : Prop := True\n"}`)
	var out, errOut bytes.Buffer
	// No --index-dsn-file and no DECLINDEX_DSN: the configuration names no way to reach the index.
	code := run(context.Background(), []string{"--config", config, "gate", "--proposer", "function:probe", proposal},
		func(string) string { return "" }, &out, &errOut)
	if code != 2 {
		t.Fatalf("gate without a connection string: exit %d, want 2\n%s", code, errOut.String())
	}
}

// --index overrides index: (P-1), so a name the file gets wrong must not refuse a right one on the command line.
// A wrong name that a command does use is a usage error (2), as a wrong flag is, not a condition a refresh
// repairs (3).
func TestAWrongIndexNameInTheFileDoesNotRefuseTheFlag(t *testing.T) {
	config := fixture(t)
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: Foo-Bar\nstate: state\nroots: [Golden, Sub]\n")
	if r := invoke(t, "--config", config, "--index", "idx", "refresh"); r.code != 0 {
		t.Fatalf("a refresh of the index the flag names: %+v", r)
	}
	if r := invoke(t, "--config", config, "status"); r.code != 2 {
		t.Fatalf("a command that uses the file's wrong index name: %+v", r)
	}
}
