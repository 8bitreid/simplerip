// Package notify sends Discord webhook messages via n8n.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// RipPayload is the JSON body POSTed to the n8n webhook after a successful
// rsync delivery. n8n formats it as a Discord embed with action buttons when
// user input is needed (extras, ambiguous), or as a plain completion notice.
type RipPayload struct {
	JobID    string    `json:"job_id"`
	Event    string    `json:"event"` // "rip_complete", "extras_ready", "ambiguous"
	Title    string    `json:"title"`
	DiscName string    `json:"disc_name"`
	DestDir  string    `json:"dest_dir"`
	Files    []string  `json:"files"`
	Media    []MKVMeta `json:"media,omitempty"`
	// Actions is non-empty when the payload requires a Discord button response.
	Actions []Action `json:"actions,omitempty"`
}

// MKVMeta is per-file metadata derived from ffprobe, included in the payload
// so Discord shows codec, channel layout, and file size without a second lookup.
type MKVMeta struct {
	File       string `json:"file"`
	VideoCodec string `json:"video_codec,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	// AudioTracks lists each audio stream as "English TrueHD 7.1", etc.
	AudioTracks []string `json:"audio_tracks,omitempty"`
	SizeBytes   int64    `json:"size_bytes"`
}

// Action describes a button the user can press in Discord.
type Action struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Client sends webhook payloads to an n8n endpoint.
type Client struct {
	webhookURL string
	httpClient *http.Client
}

// NewClient returns a Client that posts to webhookURL.
// Pass an empty webhookURL to get a no-op client (useful when no webhook is configured).
func NewClient(webhookURL string) *Client {
	return &Client{
		webhookURL: webhookURL,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Send POSTs payload to the configured webhook URL.
// If WebhookURL is empty, Send is a no-op.
func (c *Client) Send(ctx context.Context, payload RipPayload) error {
	if c.webhookURL == "" {
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

// RipCompletePayload builds a "rip_complete" payload for a finished delivery.
func RipCompletePayload(jobID, title, discName, destDir string, files []string, media []MKVMeta) RipPayload {
	return RipPayload{
		JobID:    jobID,
		Event:    "rip_complete",
		Title:    title,
		DiscName: discName,
		DestDir:  destDir,
		Files:    files,
		Media:    media,
	}
}

// ExtrasPayload builds an "extras_ready" payload with yes/no action buttons.
func ExtrasPayload(jobID, title, discName, destDir string, extras []string) RipPayload {
	return RipPayload{
		JobID:    jobID,
		Event:    "extras_ready",
		Title:    title,
		DiscName: discName,
		DestDir:  destDir,
		Files:    extras,
		Actions: []Action{
			{Label: "Rip Extras", Value: "rip_extras"},
			{Label: "Skip", Value: "skip_extras"},
		},
	}
}

// AmbiguousPayload builds an "ambiguous" payload asking the user what to rip.
func AmbiguousPayload(jobID, title, discName string, candidates []string) RipPayload {
	return RipPayload{
		JobID:    jobID,
		Event:    "ambiguous",
		Title:    title,
		DiscName: discName,
		Files:    candidates,
		Actions: []Action{
			{Label: "Rip All", Value: "rip_all"},
			{Label: "Skip", Value: "skip"},
		},
	}
}
