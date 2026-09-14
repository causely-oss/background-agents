package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type slackClient struct {
	token  string
	client *http.Client
}

func newSlackClient(token string) *slackClient {
	return &slackClient{token: token, client: &http.Client{Timeout: 10 * time.Second}}
}

// PostToThread posts a message as a reply in a Slack thread.
// username/icon_emoji override causes Slack to render a distinct sender header,
// visually separating remediator replies from mediator context messages.
// If threadTS is empty, posts as a new message.
func (s *slackClient) PostToThread(channel, threadTS, text string) error {
	payload := map[string]any{
		"channel":    channel,
		"text":       text,
		"username":   "Causely Background Agent",
		"icon_emoji": ":wrench:",
	}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("slack error: %s", result.Error)
	}
	return nil
}
