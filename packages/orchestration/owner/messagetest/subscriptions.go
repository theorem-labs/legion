package messagetest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// listenerReadInterval is how often the driver reads the listener's sessions while it waits for the owners to
// subscribe: the listener serves every session of the hub, so the driver does not read it at its own poll rate.
const listenerReadInterval = time.Second

// listenerSession is one session of the listener's GET /v1/sessions: its id and the topics it registered.
type listenerSession struct {
	SessionID string   `json:"session_id"`
	Dir       string   `json:"dir"`
	Topics    []string `json:"topics"`
}

// listenerTopics reads, from the listener's GET /v1/sessions, the topics each session whose working directory holds
// dir registered, by session id. A session registers its topics with the listener after it opened its own
// subscriptions to them (pi-envoy's envoy_subscribe), so a topic listed here reaches the session.
func listenerTopics(ctx context.Context, client *http.Client, base, token, dir string) (map[string][]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/sessions?dir="+url.QueryEscape(dir), nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read the listener's sessions: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read the listener's sessions: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read the listener's sessions: listener returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var sessions []listenerSession
	if err := json.Unmarshal(body, &sessions); err != nil {
		return nil, fmt.Errorf("decode the listener's sessions: %w", err)
	}
	topics := map[string][]string{}
	for _, session := range sessions {
		if strings.Contains(session.Dir, dir) {
			topics[session.SessionID] = session.Topics
		}
	}
	return topics, nil
}
