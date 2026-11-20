package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"mini-cage-relay/internal/claude"
)

type anthropicMessageRequest struct {
	Model     string          `json:"model"`
	System    json.RawMessage `json:"system,omitempty"`
	Stream    bool            `json:"stream"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	rep, err := s.checker.CheckAuthorized(time.Now().UTC())
	if err != nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
		return
	}

	var req anthropicMessageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "无效的 JSON 请求体: "+err.Error())
		return
	}
	sysPrompt := ""
	if len(req.System) > 0 {
		sysPrompt = extractTextContent(req.System)
	}
	msgs := make([]claude.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, claude.Message{
			Role:    m.Role,
			Content: extractTextContent(m.Content),
		})
	}

	msgID := "msg_" + randomHex(12)

	if !req.Stream {
		res, accID, err := s.executeWithFailover(r.Context(), rep, req.Model, sysPrompt, msgs, nil)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		w.Header().Set("X-Relay-Account", accID)
		w.Header().Set("X-Relay-Exit-IP", rep.ExitIP)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          msgID,
			"type":        "message",
			"role":        "assistant",
			"model":       res.Model,
			"stop_reason": "end_turn",
			"content": []map[string]string{
				{"type": "text", "text": res.Text},
			},
			"usage": map[string]int{
				"input_tokens":  res.Usage.InputTokens,
				"output_tokens": res.Usage.OutputTokens,
			},
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Relay-Exit-IP", rep.ExitIP)

	modelName := claude.NormalizeModel(req.Model, s.runner.DefaultModel)
	sendSSE := func(event string, data any) error {
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	started := false
	ensureStarted := func() error {
		if started {
			return nil
		}
		started = true
		if err := sendSSE("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      msgID,
				"type":    "message",
				"role":    "assistant",
				"model":   modelName,
				"content": []any{},
				"usage":   map[string]int{"input_tokens": 0, "output_tokens": 0},
			},
		}); err != nil {
			return err
		}
		return sendSSE("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
	}

	res, _, err := s.executeWithFailover(r.Context(), rep, req.Model, sysPrompt, msgs, func(delta string) error {
		if err := ensureStarted(); err != nil {
			return err
		}
		return sendSSE("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]string{"type": "text_delta", "text": delta},
		})
	})
	if err != nil {
		_ = sendSSE("error", map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": err.Error()},
		})
		return
	}
	if !started {
		_ = ensureStarted()
	}
	_ = sendSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	_ = sendSSE("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": res.Usage.OutputTokens},
	})
	_ = sendSSE("message_stop", map[string]any{"type": "message_stop"})
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}
