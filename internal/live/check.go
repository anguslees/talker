package live

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/genai"
)

type CheckResult struct {
	Model      string
	SetupTime  time.Duration
	FirstAudio time.Duration
	AudioBytes int
}

// Check verifies the same constrained setup used by the browser, without executing tools.
func (b *Broker) Check(ctx context.Context) (CheckResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := b.Mint(ctx, "")
	if err != nil {
		return CheckResult{}, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	started := time.Now()
	conn, response, err := dialer.DialContext(ctx, token.URL, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return CheckResult{}, errors.New("Gemini constrained WebSocket handshake failed")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadLimit(8 << 20)
	conn.SetReadDeadline(time.Now().Add(25 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(map[string]any{"setup": token.Setup}); err != nil {
		return CheckResult{}, errors.New("could not send Live setup")
	}
	var result CheckResult
	result.Model = b.config.Model
	var promptTime time.Time
	for {
		var msg genai.LiveServerMessage
		if err := conn.ReadJSON(&msg); err != nil {
			var closeError *websocket.CloseError
			if errors.As(err, &closeError) {
				return result, fmt.Errorf("Live closed with code %d (check model availability and setup configuration)", closeError.Code)
			}
			return result, errors.New("Live check ended before an audio response completed")
		}
		if msg.SetupComplete != nil {
			result.SetupTime = time.Since(started)
			promptTime = time.Now()
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteJSON(map[string]any{"realtimeInput": map[string]string{"text": "Say hello in one short sentence. Do not call tools."}}); err != nil {
				return result, errors.New("could not send test prompt")
			}
		}
		if msg.ToolCall != nil {
			return result, errors.New("model requested a tool during check; no tool was executed")
		}
		if content := msg.ServerContent; content != nil {
			if content.ModelTurn != nil {
				for _, part := range content.ModelTurn.Parts {
					if part.InlineData != nil {
						if result.AudioBytes == 0 {
							result.FirstAudio = time.Since(promptTime)
						}
						result.AudioBytes += len(part.InlineData.Data)
					}
				}
			}
			if content.TurnComplete {
				if result.AudioBytes == 0 {
					return result, errors.New("Live completed without audio")
				}
				return result, nil
			}
		}
	}
}
