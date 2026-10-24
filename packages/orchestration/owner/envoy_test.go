package owner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		if body.Topic != "notifications.lxspec.node.function:ns_match.needs" || body.Message != "need changed" || body.IdempotencyKey != "need:abc" {
			t.Fatalf("publish body = %#v", body)
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(body.Payload), &payload); err != nil || payload["item"] != "need:abc" {
			t.Fatalf("payload = %q: %v", body.Payload, err)
		}
	}))
	defer server.Close()

	if err := NewHTTPPublisher(server.URL, "envoy-bearer").Publish(context.Background(), "notifications.lxspec.node.function:ns_match.needs", "need changed", map[string]string{"item": "need:abc"}, "need:abc"); err != nil {
		t.Fatal(err)
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
	if err := publisher.Publish(context.Background(), "notifications.lxspec.all", "changed", nil, "key"); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), "lxspec.all", "changed", nil, "key"); err == nil {
		t.Fatal("Publish accepted a topic outside notifications.")
	}
	if err := publisher.Publish(context.Background(), "notifications.lxspec.all", "changed", nil, ""); err == nil || !strings.Contains(err.Error(), "idempotency key") {
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

	err := NewHTTPPublisher(server.URL, "").Publish(context.Background(), "notifications.lxspec.all", "changed", nil, "key")
	if err == nil || !strings.Contains(err.Error(), "unknown topic") {
		t.Fatalf("Publish error = %v, want listener error", err)
	}
}

// The listener answers a publish with the envelope it sent; its event_id is the id a subscribed session's delivery
// carries, so the message test can match each record of a delivery to its publication.
func TestHTTPPublisherPublishEventReturnsTheListenersEventID(t *testing.T) {
	reply := `{"event_id":"evt-9","topic":"notifications.lxspec.all","dedupe_key":"publish.key"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	defer server.Close()
	publisher := NewHTTPPublisher(server.URL, "")
	id, err := publisher.PublishEvent(context.Background(), "notifications.lxspec.all", "changed", nil, "key")
	if err != nil || id != "evt-9" {
		t.Fatalf("PublishEvent = %q, %v, want evt-9", id, err)
	}
	reply = `{"topic":"notifications.lxspec.all"}`
	if _, err := publisher.PublishEvent(context.Background(), "notifications.lxspec.all", "changed", nil, "key"); err == nil || !strings.Contains(err.Error(), "event_id") {
		t.Fatalf("PublishEvent with no event_id error = %v", err)
	}
}

func TestListenerNATSURLsUseEnvironmentThenConfig(t *testing.T) {
	config := filepath.Join(t.TempDir(), "envoy.json")
	if err := os.WriteFile(config, []byte(`{"envoy":{"url":"http://configured.test","natsUrls":["nats://a:4222","nats://b:4222"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ENVOY_CONFIG_FILE": config}
	getenv := func(name string) string { return env[name] }
	if urls, err := ListenerNATSURLs(getenv); err != nil || strings.Join(urls, ",") != "nats://a:4222,nats://b:4222" {
		t.Fatalf("ListenerNATSURLs from the configuration = %v, %v", urls, err)
	}
	env["ENVOY_NATS_URL"] = "nats://c:4222, nats://d:4222"
	if urls, err := ListenerNATSURLs(getenv); err != nil || strings.Join(urls, ",") != "nats://c:4222,nats://d:4222" {
		t.Fatalf("ListenerNATSURLs from ENVOY_NATS_URL = %v, %v", urls, err)
	}
	env = map[string]string{"ENVOY_CONFIG_FILE": filepath.Join(t.TempDir(), "missing.json")}
	if urls, err := ListenerNATSURLs(getenv); err != nil || len(urls) != 0 {
		t.Fatalf("ListenerNATSURLs with no configuration = %v, %v", urls, err)
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
		url, token, err := ResolveListener(func(name string) string {
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
		url, token, err := ResolveListener(func(name string) string {
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
	_, _, err := ResolveListener(func(name string) string {
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
		url, token, err := ResolveListener(func(name string) string {
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
		url, token, err := ResolveListener(func(name string) string {
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
			url, token, err := ResolveListener(func(name string) string { return env[name] })
			if err != nil || url != "http://configured.test" || token != "configured-token" {
				t.Fatalf("ResolveListener = (%q, %q, %v)", url, token, err)
			}
		})
	}
}

func TestResolveListenerAllowsUnauthenticatedNamedListenerWithoutConfiguration(t *testing.T) {
	url, token, err := ResolveListener(func(name string) string {
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
	_, _, err := ResolveListener(func(name string) string {
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
	_, _, err := ResolveListener(func(name string) string {
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
	_, _, err := ResolveListener(func(name string) string {
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
	_, _, err := ResolveListener(func(name string) string {
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
			_, _, err := ResolveListener(func(name string) string {
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
