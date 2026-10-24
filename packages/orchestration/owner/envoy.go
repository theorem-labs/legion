package owner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	listenerRequestTimeout = 10 * time.Second
	listenerReplyLimit     = 64 << 10 // the envelope a publish returns, or the listener's error
	tokenCommandTimeout    = 10 * time.Second
)

// Publisher delivers an owner notice through the Envoy listener.
type Publisher interface {
	Publish(ctx context.Context, topic, message string, payload any, key string) error
}

// HTTPPublisher publishes notices through the listener HTTP API.
type HTTPPublisher struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPPublisher creates a publisher for one listener URL. An empty token sends no Authorization header.
func NewHTTPPublisher(url, token string) *HTTPPublisher {
	return &HTTPPublisher{
		baseURL: strings.TrimRight(url, "/"),
		token:   token,
		client:  &http.Client{Timeout: listenerRequestTimeout},
	}
}

// Publish posts the listener envelope and preserves its idempotency key. An empty key is refused: the
// listener would give the notice a fresh dedupe key, so a repeated step would deliver it again.
func (p *HTTPPublisher) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	_, err := p.post(ctx, topic, message, payload, key)
	return err
}

// PublishEvent publishes as Publish does and returns the event_id of the envelope the listener sent: the id each
// subscribed session's delivery carries.
func (p *HTTPPublisher) PublishEvent(ctx context.Context, topic, message string, payload any, key string) (string, error) {
	reply, err := p.post(ctx, topic, message, payload, key)
	if err != nil {
		return "", err
	}
	var envelope struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(reply, &envelope); err != nil || envelope.EventID == "" {
		return "", fmt.Errorf("the listener's reply to the notice on %q names no event_id: %s", topic, strings.TrimSpace(string(reply)))
	}
	return envelope.EventID, nil
}

func (p *HTTPPublisher) post(ctx context.Context, topic, message string, payload any, key string) ([]byte, error) {
	if !strings.HasPrefix(topic, "notifications.") {
		return nil, fmt.Errorf("refuse Envoy topic %q: it must start with notifications.", topic)
	}
	if key == "" {
		return nil, fmt.Errorf("refuse Envoy notice on %q: it has no idempotency key", topic)
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Envoy payload: %w", err)
	}
	body, err := json.Marshal(struct {
		Topic          string `json:"topic"`
		Message        string `json:"message"`
		Payload        string `json:"payload"`
		IdempotencyKey string `json:"idempotency_key"`
	}{
		Topic: topic, Message: message, Payload: string(encodedPayload), IdempotencyKey: key,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Envoy publish request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Envoy publish request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	// The key makes the POST replayable to Go's transport, which then sends it again on a new connection when the
	// listener closed the reused one (its idle timeout) before answering. The listener dedupes on the body's key, so
	// a second send delivers nothing twice.
	request.Header.Set("Idempotency-Key", key)
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("publish Envoy notice: %w", err)
	}
	defer response.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(response.Body, listenerReplyLimit))
	if response.StatusCode == http.StatusOK {
		return reply, nil
	}
	detail := strings.TrimSpace(string(reply))
	var listenerError struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(reply, &listenerError) == nil && listenerError.Error != "" {
		detail = listenerError.Error
	}
	return nil, fmt.Errorf("publish Envoy notice: listener returned %s: %s", response.Status, detail)
}

// ResolveListener applies the owner command's listener URL and bearer precedence.
func ResolveListener(getenv func(string) string) (url, token string, err error) {
	namedURL := strings.TrimSpace(getenv("ENVOY_URL"))
	tokenFile := strings.TrimSpace(getenv("ENVOY_TOKEN_FILE"))
	namedToken := strings.TrimSpace(getenv("ENVOY_TOKEN"))

	var configured envoySettings
	if namedURL == "" || (tokenFile == "" && namedToken == "") {
		configured, err = loadEnvoySettings(getenv)
		// The configuration is read here with ENVOY_URL set only as a token source, so a missing file then means no token.
		if err != nil && (namedURL == "" || !errors.Is(err, os.ErrNotExist)) {
			return "", "", err
		}
	}
	url = namedURL
	if url == "" {
		url = configured.URL
	}
	if url == "" {
		return "", "", fmt.Errorf("Envoy URL is unset in ENVOY_URL and its configuration")
	}
	if _, err := listenerURL(url); err != nil {
		return "", "", err
	}

	switch {
	case tokenFile != "":
		token, err = readTokenFile(tokenFile)
		if err != nil {
			return "", "", err
		}
	case namedToken != "":
		token = namedToken
	case configured.Token != "":
		if configured.URL == "" {
			return "", "", errors.New("envoy.token needs an envoy.url when ENVOY_URL is set; use ENVOY_TOKEN or ENVOY_TOKEN_FILE")
		}
		sameOrigin, err := sameListenerOrigin(url, configured.URL)
		if err != nil {
			return "", "", err
		}
		if !sameOrigin {
			return "", "", errors.New("envoy.token cannot authenticate ENVOY_URL for another listener; use ENVOY_TOKEN or ENVOY_TOKEN_FILE")
		}
		token = configured.Token
	}
	if strings.HasPrefix(token, "!") {
		token, err = commandToken(token[1:])
		if err != nil {
			return "", "", err
		}
	}
	return url, token, nil
}

// ListenerNATSURLs are the NATS servers of the listener's hub: ENVOY_NATS_URL (comma-separated), else the Envoy
// configuration's natsUrls. No configuration means none.
func ListenerNATSURLs(getenv func(string) string) ([]string, error) {
	if named := strings.TrimSpace(getenv("ENVOY_NATS_URL")); named != "" {
		var urls []string
		for _, url := range strings.Split(named, ",") {
			if url = strings.TrimSpace(url); url != "" {
				urls = append(urls, url)
			}
		}
		return urls, nil
	}
	configured, err := loadEnvoySettings(getenv)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return configured.NATSURLs, err
}

type envoySettings struct {
	URL      string
	Token    string
	NATSURLs []string
}

// CheckEnvoyConfig reads the Envoy configuration at path as ResolveListener reads ENVOY_CONFIG_FILE, for a command that
// was told to use that file: a file it cannot read or decode is an error.
func CheckEnvoyConfig(path string) error {
	_, err := loadEnvoySettings(func(name string) string {
		if name == "ENVOY_CONFIG_FILE" {
			return path
		}
		return ""
	})
	return err
}

func loadEnvoySettings(getenv func(string) string) (envoySettings, error) {
	path, err := envoyConfigPath(getenv)
	if err != nil {
		return envoySettings{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return envoySettings{}, fmt.Errorf("read Envoy configuration %s: %w", path, err)
	}
	var file struct {
		Envoy json.RawMessage `json:"envoy"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return envoySettings{}, fmt.Errorf("decode Envoy configuration %s: %w", path, err)
	}
	if len(file.Envoy) == 0 || bytes.Equal(bytes.TrimSpace(file.Envoy), []byte("null")) {
		return envoySettings{}, nil
	}
	var section struct {
		Enabled  *bool    `json:"enabled"`
		URL      string   `json:"url"`
		NATSURLs []string `json:"natsUrls"`
		Token    string   `json:"token"`
		NATSSeed string   `json:"natsSeed"`
	}
	decoder := json.NewDecoder(bytes.NewReader(file.Envoy))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&section); err != nil {
		return envoySettings{}, fmt.Errorf("decode Envoy configuration %s: %w", path, err)
	}
	return envoySettings{URL: strings.TrimSpace(section.URL), Token: strings.TrimSpace(section.Token), NATSURLs: section.NATSURLs}, nil
}

func envoyConfigPath(getenv func(string) string) (string, error) {
	if path := strings.TrimSpace(getenv("ENVOY_CONFIG_FILE")); path != "" {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("ENVOY_CONFIG_FILE must be absolute")
		}
		return path, nil
	}
	if configHome := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); configHome != "" {
		if !filepath.IsAbs(configHome) {
			return "", fmt.Errorf("XDG_CONFIG_HOME must be absolute")
		}
		return filepath.Join(configHome, "legion", "envoy.json"), nil
	}
	home := strings.TrimSpace(getenv("HOME"))
	if home == "" {
		return "", fmt.Errorf("ENVOY_CONFIG_FILE is unset and HOME is empty")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("HOME must be absolute")
	}
	return filepath.Join(home, ".config", "legion", "envoy.json"), nil
}

func sameListenerOrigin(first, second string) (bool, error) {
	firstURL, err := listenerURL(first)
	if err != nil {
		return false, err
	}
	secondURL, err := listenerURL(second)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(firstURL.Scheme, secondURL.Scheme) && strings.EqualFold(firstURL.Host, secondURL.Host), nil
}

func listenerURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("Envoy URL must be an http or https URL without user credentials")
	}
	return parsed, nil
}

func readTokenFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read ENVOY_TOKEN_FILE %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func commandToken(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenCommandTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/bin/sh", "-c", command).Output()
	if err != nil {
		return "", fmt.Errorf("run Envoy token command: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}
