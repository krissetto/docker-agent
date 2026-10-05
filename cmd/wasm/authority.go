package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/api"
)

// authorityClient is transport-only: execution and lifecycle ownership stay on the server.
type authorityClient struct {
	base  string
	token string
	http  *http.Client
}

func newAuthorityClient(base, token string) (*authorityClient, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("authority URL must be an HTTP(S) origin or base path without credentials, query, or fragment")
	}
	return &authorityClient{base: strings.TrimRight(base, "/") + api.SessionAPIPath, token: token, http: http.DefaultClient}, nil
}

func (c *authorityClient) request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("authority: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if len(data) == 0 {
		return json.RawMessage("null"), nil
	}
	if !json.Valid(data) {
		return nil, errors.New("authority returned invalid JSON")
	}
	return data, nil
}

type authoritySnapshot = api.SessionSnapshot[string, string, json.RawMessage]

type authorityMessage = api.SessionStreamMessage[string, string, json.RawMessage]

// observe preserves canonical messages, assembles chunked snapshots, and exposes
// gaps as resnapshot barriers rather than presenting stale events as new work.
func (c *authorityClient) observe(ctx context.Context, id string, since *uint64, epoch string, onMessage func(authorityMessage) bool) error {
	path := "/" + url.PathEscape(id) + "/events"
	if since != nil {
		q := url.Values{"since": {strconv.FormatUint(*since, 10)}}
		if epoch != "" {
			q.Set("since_epoch", epoch)
		}
		path += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Fetch body reads do not observe request cancellation after headers arrive.
	stopClose := context.AfterFunc(ctx, func() { _ = resp.Body.Close() })
	defer stopClose()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("observe: %s", resp.Status)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	var chunks []byte
	var assembling bool
	var data []string
	var frameBytes int
	var chunkCursor uint64
	var baseline *authoritySnapshot
	var ready bool
	var lastSequence uint64
	consume := func() (bool, error) {
		if len(data) == 0 {
			return true, nil
		}
		var msg authorityMessage
		err := json.Unmarshal([]byte(strings.Join(data, "\n")), &msg)
		data = nil
		frameBytes = 0
		if err != nil {
			return false, fmt.Errorf("decode authority stream: %w", err)
		}
		if msg.Version != api.SessionAPIVersion {
			return false, errors.New("unsupported authority stream version")
		}
		if assembling && msg.Type != "snapshot_chunk" && msg.Type != "snapshot_end" {
			return false, errors.New("interleaved chunked snapshot")
		}
		switch msg.Type {
		case "snapshot_begin":
			if baseline != nil || ready {
				return false, errors.New("unexpected snapshot beginning")
			}
			chunks = nil
			assembling = true
			chunkCursor = msg.Cursor
			return true, nil
		case "snapshot_chunk":
			if !assembling || msg.Cursor != chunkCursor || len(chunks)+len(msg.Chunk) > 64<<20 {
				return false, errors.New("invalid or oversized chunked snapshot")
			}
			chunks = append(chunks, msg.Chunk...)
			return true, nil
		case "snapshot_end":
			if !assembling || msg.Cursor != chunkCursor {
				return false, errors.New("invalid snapshot ending")
			}
			var snap authoritySnapshot
			if err := json.Unmarshal(chunks, &snap); err != nil {
				return false, err
			}
			if snap.Cursor != chunkCursor {
				return false, errors.New("snapshot cursor differs from chunks")
			}
			assembling = false
			chunks = nil
			msg.Type = "snapshot"
			msg.Snapshot = &snap
		}
		switch msg.Type {
		case "snapshot":
			snap := msg.Snapshot
			if baseline != nil || ready || snap == nil || snap.Session == nil || snap.Session.ID != id || snap.Status.SessionID != id || snap.Epoch == "" {
				return false, errors.New("invalid authority snapshot identity")
			}
			for _, interaction := range snap.Interactions {
				if interaction.SessionID != id || interaction.InteractionID == "" {
					return false, errors.New("invalid snapshot interaction identity")
				}
				var event struct {
					SessionID string `json:"session_id"`
					RequestID string `json:"request_id"`
				}
				if err := json.Unmarshal(interaction.Event, &event); err != nil {
					return false, err
				}
				if (event.SessionID != "" && event.SessionID != id) || (event.RequestID != "" && event.RequestID != interaction.InteractionID) {
					return false, errors.New("invalid snapshot event identity")
				}
			}
			baseline = snap
			lastSequence = snap.Cursor
			if since != nil && epoch == snap.Epoch {
				lastSequence = *since
			}
		case "event":
			envelope := msg.Envelope
			if baseline == nil || envelope == nil || envelope.Version != api.SessionAPIVersion || envelope.SessionID != id || envelope.Epoch != baseline.Epoch {
				return false, errors.New("invalid authority envelope identity")
			}
			if envelope.Gap {
				onMessage(msg)
				return false, nil
			}
			var event struct {
				Type          string `json:"type"`
				SessionID     string `json:"session_id"`
				RequestID     string `json:"request_id"`
				InteractionID string `json:"interaction_id"`
			}
			if err := json.Unmarshal(envelope.Event, &event); err != nil {
				return false, err
			}
			if event.Type == "" || (event.SessionID != "" && event.SessionID != id) {
				return false, errors.New("invalid event session identity")
			}
			switch event.Type {
			case "tool_call_confirmation", "max_iterations_reached", "elicitation_request", "interaction_resolved":
				if envelope.InteractionID == "" || (event.RequestID != "" && event.RequestID != envelope.InteractionID) || (event.InteractionID != "" && event.InteractionID != envelope.InteractionID) {
					return false, errors.New("invalid event interaction identity")
				}
			}
			if event.Type == "interaction_resolved" && (event.SessionID != id || event.InteractionID != envelope.InteractionID) {
				return false, errors.New("invalid interaction resolution identity")
			}
			if envelope.Sequence == 0 {
				if ready || envelope.TurnID != "" || envelope.InteractionID != "" || envelope.TranscriptPosition != -1 {
					return false, errors.New("invalid live seed")
				}
				switch event.Type {
				case "stream_started", "agent_choice_reasoning", "agent_choice", "partial_tool_call", "tool_call", "tool_call_output":
				default:
					return false, errors.New("invalid live seed event")
				}
			} else {
				if envelope.Sequence != lastSequence+1 {
					return false, errors.New("invalid authority event sequence")
				}
				if ready && envelope.Sequence <= baseline.Cursor {
					return false, errors.New("stale authority live event")
				}
				lastSequence = envelope.Sequence
			}
		case "ready":
			if baseline == nil || ready || msg.Cursor != baseline.Cursor {
				return false, errors.New("invalid authority ready cursor")
			}
			ready = true
			if lastSequence < baseline.Cursor {
				lastSequence = baseline.Cursor
			}
		default:
			return false, errors.New("unexpected authority stream message")
		}
		return onMessage(msg), nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			more, err := consume()
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			frameBytes += len(value)
			if frameBytes > 8<<20 {
				return errors.New("oversized authority SSE frame")
			}
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return io.EOF
}
