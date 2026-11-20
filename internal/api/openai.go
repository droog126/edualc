package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"mini-cage-relay/internal/claude"
)

type openAIChatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func extractTextContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out []string
		for _, p := range parts {
			if p.Text != "" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	return strings.TrimSpace(string(raw))
}

func (s *Server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := []string{"sonnet", "opus", "haiku", "claude-sonnet-4-5", "claude-opus-4-5", "claude-haiku-4-5"}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m,
			"object":   "model",
			"created":  now,
			"owned_by": "anthropic",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rep, err := s.checker.CheckAuthorized(time.Now().UTC())
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	var req openAIChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "无效的 JSON 请求体: "+err.Error())
		return
	}
	msgs := make([]claude.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, claude.Message{
			Role:    m.Role,
			Content: extractTextContent(m.Content),
		})
	}

	chatID := "chatcmpl-" + randomHex(10)
	created := time.Now().Unix()

	if !req.Stream {
		res, accID, err := s.executeWithFailover(r.Context(), rep, req.Model, "", msgs, nil)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("X-Relay-Account", accID)
		w.Header().Set("X-Relay-Exit-IP", rep.ExitIP)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      chatID,
			"object":  "chat.completion",
			"created": created,
			"model":   res.Model,
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": res.Text,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     res.Usage.InputTokens,
				"completion_tokens": res.Usage.OutputTokens,
				"total_tokens":      res.Usage.InputTokens + res.Usage.OutputTokens,
			},
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Relay-Exit-IP", rep.ExitIP)

	modelName := claude.NormalizeModel(req.Model, s.runner.DefaultModel)
	sentHeader := false
	sendChunk := func(delta map[string]any, finishReason any) error {
		payload := map[string]any{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         delta,
					"finish_reason": finishReason,
				},
			},
		}
		b, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	_, _, err = s.executeWithFailover(r.Context(), rep, req.Model, "", msgs, func(delta string) error {
		if !sentHeader {
			sentHeader = true
			if err := sendChunk(map[string]any{"role": "assistant"}, nil); err != nil {
				return err
			}
		}
		return sendChunk(map[string]any{"content": delta}, nil)
	})
	if err != nil {
		if !sentHeader {
			// Still able to emit clean SSE error chunk.
			_ = sendChunk(map[string]any{"role": "assistant", "content": "[Relay Error] " + err.Error()}, "stop")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	_ = sendChunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "relay_error",
		},
	})
}
