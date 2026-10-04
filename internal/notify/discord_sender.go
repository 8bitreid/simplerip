package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DiscordSender posts Message values to a Discord webhook as embeds.
type DiscordSender struct {
	webhookURL string
	httpClient *http.Client
}

// NewDiscordSender returns a Sender for a Discord webhook URL.
func NewDiscordSender(webhookURL string) *DiscordSender {
	return &DiscordSender{
		webhookURL: webhookURL,
		httpClient: &http.Client{Timeout: sendTimeout},
	}
}

type discordPayload struct {
	Embeds          []discordEmbed `json:"embeds"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	URL         string         `json:"url,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Timestamp   string         `json:"timestamp"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

var eventStyle = map[Event]struct {
	heading string
	color   int
}{
	EventNeedsInput:       {"Input needed", 0xF1C40F},
	EventMultiTitle:       {"Multi-title disc", 0x3498DB},
	EventComplete:         {"Rip complete", 0x2ECC71},
	EventFailed:           {"Rip failed", 0xE74C3C},
	EventDurationMismatch: {"Duration mismatch", 0xE67E22},
}

// Send renders msg and POSTs it. Mentions are disabled so a disc label like
// "@everyone" cannot ping the channel.
func (d *DiscordSender) Send(ctx context.Context, msg Message) error {
	style, ok := eventStyle[msg.Event]
	if !ok {
		style.heading, style.color = string(msg.Event), 0x95A5A6
	}

	desc := msg.Summary
	for _, line := range msg.Details {
		desc += "\n• " + line
	}

	embed := discordEmbed{
		Title:       truncate(style.heading, 256),
		Description: truncate(desc, 4000),
		URL:         msg.Link,
		Color:       style.color,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	for _, f := range []discordField{
		{"Disc", msg.Disc, true},
		{"Device", msg.Device, true},
		{"Title", msg.Title, true},
	} {
		if strings.TrimSpace(f.Value) != "" {
			f.Value = truncate(f.Value, 1024)
			embed.Fields = append(embed.Fields, f)
		}
	}
	if msg.Link != "" {
		embed.Fields = append(embed.Fields, discordField{"UI", msg.Link, false})
	}

	payload := discordPayload{Embeds: []discordEmbed{embed}}
	payload.AllowedMentions.Parse = []string{}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal discord payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build discord request: %w", scrubError(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post discord webhook: %w", scrubError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook returned %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
