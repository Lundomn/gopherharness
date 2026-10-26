package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Tool JSON is assembled privately and accepted only after message_stop.
// A half-received write call is never executable, even when JSON looks valid.
func (c *anthropicClient) ChatStream(ctx context.Context, req ChatRequest, delta func(string) error) (Completion, error) {
	body, err := c.chatBody(req, true)
	if err != nil {
		return Completion{}, err
	}
	blocks := map[int]map[string]any{}
	partial := map[int]string{}
	stopped := map[int]bool{}
	var p anthropicResponse
	var visible string
	done := false
	err = c.stream(ctx, "/messages", body, map[string]string{"x-api-key": c.profile.APIKey, "anthropic-version": "2023-06-01"}, func(data []byte) error {
		var e struct {
			Type    string            `json:"type"`
			Index   int               `json:"index"`
			Message anthropicResponse `json:"message"`
			Block   map[string]any    `json:"content_block"`
			Delta   struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				JSON       string `json:"partial_json"`
				Thinking   string `json:"thinking"`
				Signature  string `json:"signature"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage WireUsage `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if x := json.Unmarshal(data, &e); x != nil {
			return x
		}
		if done {
			return fmt.Errorf("event after message stopped")
		}
		switch e.Type {
		case "message_start":
			p = e.Message
		case "content_block_start":
			if e.Index < 0 || e.Index != len(blocks) || e.Block == nil {
				return fmt.Errorf("invalid stream block index %d", e.Index)
			}
			blocks[e.Index] = e.Block
		case "content_block_delta":
			b, ok := blocks[e.Index]
			if !ok || stopped[e.Index] {
				return fmt.Errorf("delta without active block")
			}
			switch e.Delta.Type {
			case "text_delta":
				b["text"] = stringField(b, "text") + e.Delta.Text
				visible += e.Delta.Text
				return delta(e.Delta.Text)
			case "input_json_delta":
				partial[e.Index] += e.Delta.JSON
			case "thinking_delta":
				b["thinking"] = stringField(b, "thinking") + e.Delta.Thinking
			case "signature_delta":
				b["signature"] = stringField(b, "signature") + e.Delta.Signature
			}
		case "content_block_stop":
			b, ok := blocks[e.Index]
			if !ok || stopped[e.Index] {
				return fmt.Errorf("stop without active block")
			}
			if raw, ok := partial[e.Index]; ok {
				args, x := nativeArgs(raw)
				if x != nil {
					return x
				}
				b["input"] = args
			}
			stopped[e.Index] = true
		case "message_delta":
			p.Usage.Output = e.Usage.Output
			p.StopReason = e.Delta.StopReason
		case "message_stop":
			if len(stopped) != len(blocks) {
				return fmt.Errorf("unfinished content blocks")
			}
			done = true
		case "error":
			return fmt.Errorf("provider stream error: %s", e.Error.Message)
		}
		return nil
	})
	if err == nil && !done {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return Completion{Text: visible, Usage: p.Usage.Normalized(false), ActualModel: p.Model}, err
	}
	p.Content = nil
	for i := 0; i < len(blocks); i++ {
		raw, e := json.Marshal(blocks[i])
		if e != nil {
			return Completion{}, e
		}
		p.Content = append(p.Content, raw)
	}
	return parseAnthropic(p)
}
func stringField(m map[string]any, k string) string { s, _ := m[k].(string); return s }
