package hermes

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

const (
	maxSSELineBytes  = 64 << 10
	maxSSEEventBytes = 256 << 10
	streamIdleLimit  = 45 * time.Second
)

type RunEvent struct {
	ID        string          `json:"-"`
	Event     string          `json:"event"`
	RunID     string          `json:"run_id"`
	Seq       *int64          `json:"seq,omitempty"`
	Timestamp float64         `json:"timestamp"`
	Delta     string          `json:"delta,omitempty"`
	Output    string          `json:"output,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
}

// StreamEvents makes one streaming request. A disconnect never cancels the run.
// Callers must reconcile with GetRun: older Hermes releases cannot replay SSE.
func (c *Client) StreamEvents(ctx context.Context, runID, lastEventID string, receive func(RunEvent) error) error {
	if !validID(runID) || receive == nil {
		return errors.New("Hermes event stream requires a valid run ID and receiver")
	}
	if len(lastEventID) > 128 || strings.IndexFunc(lastEventID, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
		return errors.New("invalid Hermes Last-Event-ID")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(streamIdleLimit, cancel)
	defer timer.Stop()
	req, err := c.request(streamCtx, http.MethodGet, "/v1/runs/"+runID+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("Hermes event stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		return errors.New("Hermes event endpoint did not return text/event-stream")
	}
	err = parseSSE(resp.Body, func() { timer.Reset(streamIdleLimit) }, func(event RunEvent) error {
		if event.RunID != "" && event.RunID != runID {
			return errors.New("Hermes event belongs to a different run")
		}
		return receive(event)
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if streamCtx.Err() != nil {
		return errors.New("Hermes event stream exceeded idle timeout")
	}
	return err
}

func parseSSE(reader io.Reader, activity func(), receive func(RunEvent) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	var data strings.Builder
	var eventName, eventID string
	frameBytes := 0
	for scanner.Scan() {
		activity()
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				var event RunEvent
				if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
					return fmt.Errorf("decode Hermes SSE event: %w", err)
				}
				if event.Event == "" {
					event.Event = eventName
				}
				event.ID = eventID
				if err := receive(event); err != nil {
					return err
				}
			}
			data.Reset()
			eventName = ""
			frameBytes = 0
			continue
		}
		frameBytes += len(line) + 1
		if frameBytes > maxSSEEventBytes {
			return errors.New("Hermes SSE event exceeds 256 KiB limit")
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		case "event":
			eventName = value
		case "id":
			if len(value) > 128 || strings.ContainsRune(value, '\x00') {
				return errors.New("invalid Hermes SSE event ID")
			}
			eventID = value
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Hermes SSE: %w", err)
	}
	// An unterminated frame is incomplete, not evidence of a completed run.
	if data.Len() != 0 {
		return io.ErrUnexpectedEOF
	}
	return nil
}
