package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
)

// ChatRequest is a complete immutable request snapshot. Tools are not prompt
// prose; calls and results retain their wire IDs across model iterations.
type ChatRequest struct {
	Messages        []model.Message `json:"messages"`
	Tools           []tools.Spec    `json:"tools,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens"`
}
type ChatClient interface {
	Chat(context.Context, ChatRequest) (Completion, error)
	NativeTools() bool
}
type ChatStreamClient interface {
	ChatStream(context.Context, ChatRequest, func(string) error) (Completion, error)
}

func HasNativeTools(c Client) bool           { n, ok := c.(ChatClient); return ok && n.NativeTools() }
func (c *openAIClient) NativeTools() bool    { return true }
func (c *anthropicClient) NativeTools() bool { return true }

// ValidateConversation rejects incomplete/reordered exchanges before sending.
// A cancelled tool batch must be closed with explicit results, not orphaned.
func ValidateConversation(messages []model.Message) error {
	pending, seen := map[string]bool{}, map[string]bool{}
	for _, m := range messages {
		if m.Role == "tool" {
			if !pending[m.ToolCallID] {
				return fmt.Errorf("orphan or duplicate tool result %q", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			continue
		}
		if len(pending) != 0 {
			return errors.New("tool batch is missing results")
		}
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("invalid message role %q", m.Role)
		}
		for _, call := range m.ToolCalls {
			if m.Role != "assistant" || call.ID == "" || call.Name == "" || seen[call.ID] {
				return errors.New("invalid or repeated tool call ID")
			}
			seen[call.ID], pending[call.ID] = true, true
		}
	}
	if len(pending) != 0 {
		return errors.New("tool batch is missing results")
	}
	return nil
}

func nativeArgs(raw string) (map[string]any, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	if args == nil {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	return args, nil
}
func validateCalls(out Completion) (Completion, error) {
	seen := map[string]bool{}
	for _, c := range out.ToolCalls {
		if c.ID == "" || c.Name == "" || seen[c.ID] || c.Args == nil {
			return out, errors.New("invalid native tool call")
		}
		seen[c.ID] = true
	}
	return out, nil
}

func (c *openAIClient) chatBody(req ChatRequest, stream bool) (map[string]any, error) {
	if err := ValidateConversation(req.Messages); err != nil {
		return nil, err
	}
	input := []any{}
	for _, m := range req.Messages {
		for _, item := range m.NativeContent {
			if item.Protocol == "openai" {
				input = append(input, item.Data)
			}
		}
		if m.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			continue
		}
		if m.Content != "" {
			input = append(input, map[string]any{"role": m.Role, "content": m.Content})
		}
		for _, call := range m.ToolCalls {
			args, err := json.Marshal(call.Args)
			if err != nil {
				return nil, err
			}
			input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(args)})
		}
	}
	defs := []any{}
	for _, s := range req.Tools {
		schema, err := tools.JSONSchema(s.Schema)
		if err != nil {
			return nil, err
		}
		defs = append(defs, map[string]any{"type": "function", "name": s.Name, "description": s.Description, "parameters": schema, "strict": false})
	}
	return map[string]any{"model": c.Model(), "input": input, "tools": defs, "max_output_tokens": req.MaxOutputTokens, "stream": stream, "store": false, "include": []string{"reasoning.encrypted_content"}}, nil
}

type nativeResponse struct {
	Model      string            `json:"model"`
	Status     string            `json:"status"`
	OutputText string            `json:"output_text"`
	Output     []json.RawMessage `json:"output"`
	Usage      WireUsage         `json:"usage"`
	Error      *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func parseResponse(p nativeResponse) (Completion, error) {
	out := Completion{ActualModel: p.Model, Usage: p.Usage.Normalized(true), StopReason: p.Status}
	if p.Error != nil {
		return out, fmt.Errorf("provider response failed: %s", p.Error.Message)
	}
	if p.Status != "" && p.Status != "completed" {
		return out, fmt.Errorf("provider response not complete: %s", p.Status)
	}
	for _, raw := range p.Output {
		var item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return out, err
		}
		switch item.Type {
		case "function_call":
			args, err := nativeArgs(item.Arguments)
			if err != nil {
				return out, err
			}
			out.ToolCalls = append(out.ToolCalls, model.ToolCall{ID: item.CallID, Name: item.Name, Args: args})
		case "message":
			for _, block := range item.Content {
				out.Text += block.Text + block.Refusal
			}
		case "reasoning":
			out.NativeContent = append(out.NativeContent, model.NativeItem{Protocol: "openai", Data: append(json.RawMessage(nil), raw...)})
		default:
			return out, fmt.Errorf("unsupported response item %q", item.Type)
		}
	}
	if out.Text == "" {
		out.Text = p.OutputText
	}
	return validateCalls(out)
}
func (c *openAIClient) Chat(ctx context.Context, req ChatRequest) (Completion, error) {
	body, err := c.chatBody(req, false)
	if err != nil {
		return Completion{}, err
	}
	var p nativeResponse
	if err = c.do(ctx, "/responses", body, map[string]string{"Authorization": "Bearer " + c.profile.APIKey}, &p); err != nil {
		return Completion{}, err
	}
	return parseResponse(p)
}
func (c *openAIClient) ChatStream(ctx context.Context, req ChatRequest, delta func(string) error) (Completion, error) {
	body, err := c.chatBody(req, true)
	if err != nil {
		return Completion{}, err
	}
	var out Completion
	done := false
	err = c.stream(ctx, "/responses", body, map[string]string{"Authorization": "Bearer " + c.profile.APIKey}, func(data []byte) error {
		var event struct {
			Type     string         `json:"type"`
			Delta    string         `json:"delta"`
			Response nativeResponse `json:"response"`
			Message  string         `json:"message"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if done {
			return fmt.Errorf("event after response completed")
		}
		switch event.Type {
		case "response.output_text.delta":
			out.Text += event.Delta
			return delta(event.Delta)
		case "response.completed":
			complete, e := parseResponse(event.Response)
			if complete.Text == "" {
				complete.Text = out.Text
			}
			out, done = complete, e == nil
			return e
		case "response.failed", "response.incomplete":
			return fmt.Errorf("provider stream %s", event.Type)
		case "error":
			return fmt.Errorf("provider stream error: %s", event.Message)
		}
		return nil
	})
	if err == nil && !done {
		err = io.ErrUnexpectedEOF
	}
	return out, err
}

func (c *anthropicClient) chatBody(req ChatRequest, stream bool) (map[string]any, error) {
	if err := ValidateConversation(req.Messages); err != nil {
		return nil, err
	}
	var system []string
	messages := []map[string]any{}
	for i, m := range req.Messages {
		if m.Role == "system" && len(messages) == 0 {
			system = append(system, m.Content)
			continue
		}
		role := m.Role
		content := []any{}
		if role == "tool" {
			role = "user"
			content = append(content, map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content})
		} else {
			if role == "system" {
				role = "user"
				m.Content = "[Runtime notice]\n" + m.Content
			}
			for _, item := range m.NativeContent {
				if item.Protocol == "anthropic" {
					content = append(content, item.Data)
				}
			}
			if m.Content != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Content})
			}
			for _, call := range m.ToolCalls {
				content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": call.Args})
			}
		}
		if len(content) == 0 {
			continue
		}
		// Consecutive tool results must be one user message before any text.
		if i > 0 && req.Messages[i-1].Role == "tool" && m.Role == "tool" && len(messages) > 0 {
			last := messages[len(messages)-1]
			last["content"] = append(last["content"].([]any), content...)
		} else {
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
	}
	defs := []any{}
	for _, s := range req.Tools {
		schema, e := tools.JSONSchema(s.Schema)
		if e != nil {
			return nil, e
		}
		defs = append(defs, map[string]any{"name": s.Name, "description": s.Description, "input_schema": schema})
	}
	return map[string]any{"model": c.Model(), "system": strings.Join(system, "\n\n"), "messages": messages, "tools": defs, "max_tokens": req.MaxOutputTokens, "stream": stream}, nil
}

type anthropicResponse struct {
	Model      string            `json:"model"`
	Content    []json.RawMessage `json:"content"`
	Usage      WireUsage         `json:"usage"`
	StopReason string            `json:"stop_reason"`
}

func parseAnthropic(p anthropicResponse) (Completion, error) {
	out := Completion{ActualModel: p.Model, Usage: p.Usage.Normalized(false), StopReason: p.StopReason}
	for _, raw := range p.Content {
		var item struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return out, err
		}
		switch item.Type {
		case "text":
			out.Text += item.Text
		case "tool_use":
			args, e := nativeArgs(string(item.Input))
			if e != nil {
				return out, e
			}
			out.ToolCalls = append(out.ToolCalls, model.ToolCall{ID: item.ID, Name: item.Name, Args: args})
		case "thinking", "redacted_thinking":
			out.NativeContent = append(out.NativeContent, model.NativeItem{Protocol: "anthropic", Data: append(json.RawMessage(nil), raw...)})
		default:
			return out, fmt.Errorf("unsupported content block %q", item.Type)
		}
	}
	if p.StopReason == "max_tokens" {
		return out, errors.New("model output token limit reached before complete response")
	}
	return validateCalls(out)
}
func (c *anthropicClient) Chat(ctx context.Context, req ChatRequest) (Completion, error) {
	body, e := c.chatBody(req, false)
	if e != nil {
		return Completion{}, e
	}
	var p anthropicResponse
	e = c.do(ctx, "/messages", body, map[string]string{"x-api-key": c.profile.APIKey, "anthropic-version": "2023-06-01"}, &p)
	if e != nil {
		return Completion{}, e
	}
	return parseAnthropic(p)
}
func (m *Metered) NativeTools() bool { return HasNativeTools(m.Delegate) }
func (m *Metered) Chat(ctx context.Context, r ChatRequest) (Completion, error) {
	c, ok := m.Delegate.(ChatClient)
	if !ok || !c.NativeTools() {
		return Completion{}, errors.New("native tools unsupported")
	}
	out, err := c.Chat(ctx, r)
	m.record(ctx, out, err, false)
	return out, err
}
func (m *Metered) ChatStream(ctx context.Context, r ChatRequest, delta func(string) error) (Completion, error) {
	var out Completion
	var err error
	if c, ok := m.Delegate.(ChatStreamClient); ok {
		out, err = c.ChatStream(ctx, r, delta)
	} else if c, ok := m.Delegate.(ChatClient); ok {
		out, err = c.Chat(ctx, r)
		if err == nil && out.Text != "" {
			err = delta(out.Text)
		}
	} else {
		err = errors.New("native tools unsupported")
	}
	m.record(ctx, out, err, true)
	return out, err
}
func (r *Resilient) NativeTools() bool {
	if len(r.Clients) == 0 {
		return false
	}
	for _, c := range r.Clients {
		if !HasNativeTools(c) {
			return false
		}
	}
	return true
}
func (r *Resilient) Chat(ctx context.Context, req ChatRequest) (Completion, error) {
	ctx = requested(ctx, r.Model())
	return r.call(ctx, func(c Client) (Completion, error) {
		n, ok := c.(ChatClient)
		if !ok || !n.NativeTools() {
			return Completion{}, errors.New("native tools unsupported")
		}
		return n.Chat(ctx, req)
	})
}
func (r *Resilient) ChatStream(ctx context.Context, req ChatRequest, delta func(string) error) (Completion, error) {
	ctx = requested(ctx, r.Model())
	out, err := r.call(ctx, func(c Client) (Completion, error) {
		emitted, callbackFailed := false, false
		emit := func(s string) error {
			emitted = emitted || s != ""
			e := delta(s)
			callbackFailed = callbackFailed || e != nil
			return e
		}
		var v Completion
		var e error
		if s, ok := c.(ChatStreamClient); ok {
			v, e = s.ChatStream(ctx, req, emit)
		} else if n, ok := c.(ChatClient); ok {
			v, e = n.Chat(ctx, req)
			if e == nil && v.Text != "" {
				e = emit(v.Text)
			}
		} else {
			e = errors.New("native tools unsupported")
		}
		if e != nil && (emitted || callbackFailed) {
			return v, partialStream{e}
		}
		return v, e
	})
	var p partialStream
	if errors.As(err, &p) {
		err = p.err
	}
	return out, err
}
