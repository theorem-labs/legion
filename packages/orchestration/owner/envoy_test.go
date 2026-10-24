package owner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHTTPPublisherSendsListenerEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/publish" {
			t.Fatalf("request = %s %s, want POST /v1/messages/publish", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer envoy-bearer" {
			t.Fatalf("Authorization = %q, want bearer", got)
		}
		var body struct {
			Topic          string `json:"topic"`
			Message        string `json:"message"`
			Payload        string `json:"payload"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Topic != "notifications.example.node.function:ns_match.needs" || body.Message != "need changed" || body.IdempotencyKey != "need:abc" {
			t.Fatalf("publish body = %#v", body)
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(body.Payload), &payload); err != nil || payload["item"] != "need:abc" {
			t.Fatalf("payload = %q: %v", body.Payload, err)
		}
	}))
	defer server.Close()

	if err := NewHTTPPublisher(server.URL, "envoy-bearer").Publish(context.Background(), "notifications.example.node.function:ns_match.needs", "need changed", map[string]string{"item": "need:abc"}, "need:abc"); err != nil {
		t.Fatal(err)
	}
}

// The listener closes a keep-alive connection that idles for its IdleTimeout (60 s). A publish that reuses such a
// connection as the listener closes it reads EOF, though the listener answered nothing. The notice carries its
// idempotency key, which the listener dedupes on, so the publish is sent again on a new connection.
func TestHTTPPublisherSendsAgainWhenTheListenerClosesAReusedConnection(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, body.IdempotencyKey)
		if len(keys) == 2 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			connection.Close()
			return
		}
		_, _ = w.Write([]byte(`{"event_id":"evt-` + body.IdempotencyKey + `"}`))
	}))
	defer server.Close()

	publisher := NewHTTPPublisher(server.URL, "")
	for _, key := range []string{"first", "second"} {
		if err := publisher.Publish(context.Background(), "notifications.example.all", "notice", map[string]string{}, key); err != nil {
			t.Fatalf("publish %s: %v", key, err)
		}
	}
	if want := []string{"first", "second", "second"}; !slices.Equal(keys, want) {
		t.Fatalf("the listener received keys %v, want %v", keys, want)
	}
}

func TestHTTPPublisherOmitsEmptyAuthorizationAndRefusesNonNotificationTopics(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want empty", got)
		}
	}))
	defer server.Close()
	publisher := NewHTTPPublisher(server.URL, "")
	if err := publisher.Publish(context.Background(), "notifications.example.all", "changed", nil, "key"); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), "example.all", "changed", nil, "key"); err == nil {
		t.Fatal("Publish accepted a topic outside notifications.")
	}
	if err := publisher.Publish(context.Background(), "notifications.example.all", "changed", nil, ""); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("Publish without an idempotency key error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one", requests)
	}
}

func TestHTTPPublisherReturnsTheListenerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unknown topic"}`))
	}))
	defer server.Close()

	err := NewHTTPPublisher(server.URL, "").Publish(context.Background(), "notifications.example.all", "changed", nil, "key")
	if err == nil || !strings.Contains(err.Error(), "unknown topic") {
		t.Fatalf("Publish error = %v, want listener error", err)
	}
}

func TestResolveListenerUsesEnvironmentThenConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "envoy.json")
	if err := os.WriteFile(config, []byte(`{"envoy":{"url":"http://configured.test","token":"configured-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("environment wins", func(t *testing.T) {
		url, token, err := ResolveListener(context.Background(), func(name string) string {
			return map[string]string{
				"ENVOY_URL":         "http://environment.test",
				"ENVOY_TOKEN_FILE":  tokenFile,
				"ENVOY_TOKEN":       "environment-token",
				"ENVOY_CONFIG_FILE": config,
			}[name]
		})
		if err != nil || url != "http://environment.test" || token != "file-token" {
			t.Fatalf("ResolveListener = (%q, %q, %v)", url, token, err)
		}
	})

	t.Run("config fallback", func(t *testing.T) {
		url, token, err := ResolveListener(context.Background(), func(name string) string {
			if name == "ENVOY_CONFIG_FILE" {
				return config
			}
			return ""
		})
		if err != nil || url != "http://configured.test" || token != "configured-token" {
			t.Fatalf("ResolveListener = (%q, %q, %v)", url, token, err)
		}
	})
}

func TestResolveListenerDoesNotSendConfiguredTokenToAnotherListener(t *testing.T) {
	config := filepath.Join(t.TempDir(), "envoy.json")
	if err := os.WriteFile(config, []byte(`{"envoy":{"url":"http://configured.test","token":"configured-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveListener(context.Background(), func(name string) string {
		return map[string]string{
			"ENVOY_URL":         "http://other.test",
			"ENVOY_CONFIG_FILE": config,
		}[name]
	})
	if err == nil || !strings.Contains(err.Error(), "ENVOY_TOKEN") {
		t.Fatalf("ResolveListener configured token for another listener error = %v", err)
	}
}

func TestResolveListenerCredentialAndConfigurationPrecedence(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "envoy.json")
	if err := os.WriteFile(config, []byte(`{"envoy":{"url":"http://configured.test","token":"configured-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("environment token wins", func(t *testing.T) {
		url, token, err := ResolveListener(context.Background(), func(name string) string {
			return map[string]string{
				"ENVOY_URL":         "http://environment.test",
				"ENVOY_TOKEN":       "environment-token",
				"ENVOY_CONFIG_FILE": config,
			}[name]
		})
		if err != nil || url != "http://environment.test" || token != "environment-token" {
			t.Fatalf("ResolveListener = (%q, %q, %v)", url, token, err)
		}
	})

	t.Run("token command resolves", func(t *testing.T) {
		url, token, err := ResolveListener(context.Background(), func(name string) string {
			return map[string]string{
				"ENVOY_URL":   "http://environment.test",
				"ENVOY_TOKEN": "!printf command-token",
			}[name]
		})
		if err != nil || url != "http://environment.test" || token != "command-token" {
			t.Fatalf("ResolveListener = (%q, %q, %v)", url, token, err)
		}
	})

	for _, tc := range []struct {
		name string
		env  func(home string) map[string]string
	}{
		{
			name: "XDG_CONFIG_HOME",
			env: func(home string) map[string]string {
				return map[string]string{"XDG_CONFIG_HOME": home}
			},
		},
		{
			name: "HOME",
			env: func(home string) map[string]string {
				return map[string]string{"HOME": home}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			configHome := home
			if tc.name == "HOME" {
				configHome = filepath.Join(home, ".config")
			}
			if err := os.MkdirAll(filepath.Join(configHome, "legion"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configHome, "legion", "envoy.json"), []byte(`{"envoy":{"url":"http://configured.test","token":"configured-token"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			env := tc.env(home)
			url, token, err := ResolveListener(context.Background(), func(name string) string { return env[name] })
			if err != nil || url != "http://configured.test" || token != "configured-token" {
				t.Fatalf("ResolveListener = (%q, %q, %v)", url, token, err)
			}
		})
	}
}

func TestResolveListenerAllowsUnauthenticatedNamedListenerWithoutConfiguration(t *testing.T) {
	url, token, err := ResolveListener(context.Background(), func(name string) string {
		return map[string]string{
			"ENVOY_URL": "http://listener.test",
			"HOME":      t.TempDir(),
		}[name]
	})
	if err != nil || url != "http://listener.test" || token != "" {
		t.Fatalf("ResolveListener = (%q, %q, %v)", url, token, err)
	}
}

func TestResolveListenerDoesNotExposeURLUserinfo(t *testing.T) {
	const secret = "top-secret-listener-token"
	_, _, err := ResolveListener(context.Background(), func(name string) string {
		return map[string]string{
			"ENVOY_URL": "http://owner:" + secret + "@listener.test",
			"HOME":      t.TempDir(),
		}[name]
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("ResolveListener userinfo error = %v, want no secret", err)
	}
}

func TestResolveListenerRejectsRelativeConfigurationPath(t *testing.T) {
	_, _, err := ResolveListener(context.Background(), func(name string) string {
		if name == "ENVOY_CONFIG_FILE" {
			return "envoy.json"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("ResolveListener relative configuration error = %v", err)
	}
}

func TestResolveListenerRejectsUnknownEnvoyConfigurationKey(t *testing.T) {
	config := filepath.Join(t.TempDir(), "envoy.json")
	if err := os.WriteFile(config, []byte(`{"envoy":{"url":"http://configured.test","tokne":"misspelled"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveListener(context.Background(), func(name string) string {
		if name == "ENVOY_CONFIG_FILE" {
			return config
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "tokne") {
		t.Fatalf("ResolveListener unknown Envoy key error = %v", err)
	}
}

func TestResolveListenerReportsMissingURLForAnEmptyConfiguration(t *testing.T) {
	config := filepath.Join(t.TempDir(), "envoy.json")
	if err := os.WriteFile(config, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveListener(context.Background(), func(name string) string {
		if name == "ENVOY_CONFIG_FILE" {
			return config
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "Envoy URL is unset") {
		t.Fatalf("ResolveListener empty configuration error = %v", err)
	}
}

func TestResolveListenerRejectsInvalidURL(t *testing.T) {
	for _, value := range []string{"ftp://listener.test", "http:///missing-host", "http://user@listener.test"} {
		t.Run(value, func(t *testing.T) {
			_, _, err := ResolveListener(context.Background(), func(name string) string {
				return map[string]string{
					"ENVOY_URL":   value,
					"ENVOY_TOKEN": "environment-token",
				}[name]
			})
			if err == nil || !strings.Contains(err.Error(), "http or https") {
				t.Fatalf("ResolveListener(%q) error = %v", value, err)
			}
		})
	}
}

// holdingTokenCommand is a token command whose shell starts a child that keeps the command's output open after the
// shell is killed, and the file that the child's PID goes to. The child sleeps 300 s, so a caller that waits for the
// output to close waits that long; the test's cleanup kills it, so a failing run leaves nothing behind.
func holdingTokenCommand(t *testing.T) (command, pidFile string) {
	t.Helper()
	pidFile = filepath.Join(t.TempDir(), "child.pid")
	t.Cleanup(func() {
		if pid, err := readPID(pidFile); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return "sleep 300 & echo $! > " + pidFile + "; wait; printf token", pidFile
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// processRunning reports whether pid is a process that has not exited: one with a /proc entry that is not a zombie.
func processRunning(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	_, after, found := strings.Cut(string(stat), ") ")
	return found && !strings.HasPrefix(after, "Z")
}

// requireChildGone fails the test unless the child whose PID is in pidFile has exited within a second.
func requireChildGone(t *testing.T, pidFile string) {
	t.Helper()
	pid, err := readPID(pidFile)
	if err != nil {
		t.Fatalf("the token command's child wrote no PID: %v", err)
	}
	for deadline := time.Now().Add(time.Second); processRunning(pid); {
		if time.Now().After(deadline) {
			t.Fatalf("the token command's child %d still runs after the command returned", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tokenWithin runs resolve in the background and fails the test unless it returns an error within limit.
func tokenWithin(t *testing.T, limit time.Duration, resolve func() error) time.Duration {
	t.Helper()
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- resolve() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the token command succeeded, want it stopped")
		}
		return time.Since(started)
	case <-time.After(limit):
		t.Fatalf("the token command still ran after %s", limit)
	}
	return 0
}

// A token command stops when its caller's context ends: its whole process group is killed, so a child that holds the
// output open dies with the shell, and the command returns within a second or so of the deadline.
func TestCommandTokenStopsWithItsChildrenAtTheCallersDeadline(t *testing.T) {
	command, pidFile := holdingTokenCommand(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	elapsed := tokenWithin(t, 6*time.Second, func() error {
		_, err := commandToken(ctx, command)
		return err
	})
	t.Logf("the token command returned %s after it started, its caller's deadline 1s", elapsed)
	requireChildGone(t, pidFile)
}

// A token command runs for at most 10 s (tokenCommandTimeout), also when its caller gives it longer, and its children
// stop with it.
func TestCommandTokenStopsWithItsChildrenAtTenSeconds(t *testing.T) {
	command, pidFile := holdingTokenCommand(t)
	elapsed := tokenWithin(t, tokenCommandTimeout+5*time.Second, func() error {
		_, err := commandToken(context.Background(), command)
		return err
	})
	if elapsed < tokenCommandTimeout {
		t.Fatalf("the token command stopped after %s, before its %s", elapsed, tokenCommandTimeout)
	}
	t.Logf("the token command returned %s after it started", elapsed)
	requireChildGone(t, pidFile)
}

// ResolveListener runs a token command under its caller's context.
func TestResolveListenerRunsTheTokenCommandUnderTheCallersContext(t *testing.T) {
	command, pidFile := holdingTokenCommand(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tokenWithin(t, 6*time.Second, func() error {
		_, _, err := ResolveListener(ctx, func(name string) string {
			return map[string]string{"ENVOY_URL": "http://listener.test", "ENVOY_TOKEN": "!" + command}[name]
		})
		return err
	})
	requireChildGone(t, pidFile)
}
