package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
)

type httpClient struct {
	profile config.Provider
	http    *http.Client
}

func NewOpenAI(p config.Provider, timeoutSeconds int) Client {
	return &openAIClient{httpClient{p, timeout(timeoutSeconds)}}
}
func NewAnthropic(p config.Provider, timeoutSeconds int) Client {
	return &anthropicClient{httpClient{p, timeout(timeoutSeconds)}}
}

func timeout(seconds int) *http.Client {
	if seconds <= 0 {
		seconds = 300
	}
	return &http.Client{Timeout: time.Duration(seconds) * time.Second}
}

type openAIClient struct{ httpClient }

type openAIResponse struct {
	OutputText string `json:"output_text"`
	Output     []struct {
		Content []struct{ Type, Text string } `json:"content"`
	} `json:"output"`
	Usage struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *openAIClient) Name() string  { return c.profile.Name }
func (c *openAIClient) Model() string { return c.profile.Model }
func (c *openAIClient) Complete(ctx context.Context, prompt string, maxTokens int) (Completion, error) {
	body := map[string]any{"model": c.profile.Model, "input": prompt, "max_output_tokens": maxTokens, "stream": false}
	var payload openAIResponse
	if err := c.do(ctx, "/responses", body, map[string]string{"Authorization": "Bearer " + c.profile.APIKey}, &payload); err != nil {
		return Completion{}, err
	}
	return openAICompletion(payload), nil
}

func (c *openAIClient) InspectImage(ctx context.Context, mime string, data []byte, prompt string, maxTokens int) (Completion, error) {
	imageURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	content := []map[string]any{{"type": "input_text", "text": prompt}, {"type": "input_image", "image_url": imageURL}}
	body := map[string]any{"model": c.profile.Model, "input": []map[string]any{{"role": "user", "content": content}}, "max_output_tokens": maxTokens, "stream": false}
	var payload openAIResponse
	if err := c.do(ctx, "/responses", body, map[string]string{"Authorization": "Bearer " + c.profile.APIKey}, &payload); err != nil {
		return Completion{}, err
	}
	return openAICompletion(payload), nil
}

func (c *openAIClient) Stream(ctx context.Context, prompt string, maxTokens int, onDelta func(string) error) (Completion, error) {
	body := map[string]any{"model": c.profile.Model, "input": prompt, "max_output_tokens": maxTokens, "stream": true}
	var result Completion
	err := c.stream(ctx, "/responses", body, map[string]string{"Authorization": "Bearer " + c.profile.APIKey}, func(data []byte) error {
		var event struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Usage struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if event.Delta != "" {
			result.Text += event.Delta
			return onDelta(event.Delta)
		}
		if event.Type == "response.completed" {
			result.Usage = Usage{InputTokens: event.Response.Usage.Input, OutputTokens: event.Response.Usage.Output}
		}
		return nil
	})
	return result, err
}

func openAICompletion(payload openAIResponse) Completion {
	text := payload.OutputText
	if text == "" {
		for _, out := range payload.Output {
			for _, block := range out.Content {
				text += block.Text
			}
		}
	}
	return Completion{Text: text, Usage: Usage{InputTokens: payload.Usage.Input, OutputTokens: payload.Usage.Output}}
}

type anthropicClient struct{ httpClient }

func (c *anthropicClient) Name() string  { return c.profile.Name }
func (c *anthropicClient) Model() string { return c.profile.Model }
func (c *anthropicClient) Complete(ctx context.Context, prompt string, maxTokens int) (Completion, error) {
	body := map[string]any{"model": c.profile.Model, "max_tokens": maxTokens, "messages": []map[string]string{{"role": "user", "content": prompt}}}
	var payload struct {
		Content []struct{ Type, Text string } `json:"content"`
		Usage   struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	headers := map[string]string{"x-api-key": c.profile.APIKey, "anthropic-version": "2023-06-01"}
	if err := c.do(ctx, "/messages", body, headers, &payload); err != nil {
		return Completion{}, err
	}
	var b strings.Builder
	for _, block := range payload.Content {
		if block.Text != "" {
			b.WriteString(block.Text)
		}
	}
	return Completion{Text: b.String(), Usage: Usage{InputTokens: payload.Usage.Input, OutputTokens: payload.Usage.Output}}, nil
}

func (c *anthropicClient) InspectImage(ctx context.Context, mime string, data []byte, prompt string, maxTokens int) (Completion, error) {
	content := []map[string]any{
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": base64.StdEncoding.EncodeToString(data)}},
		{"type": "text", "text": prompt},
	}
	body := map[string]any{"model": c.profile.Model, "max_tokens": maxTokens, "messages": []map[string]any{{"role": "user", "content": content}}}
	var payload struct {
		Content []struct{ Type, Text string } `json:"content"`
		Usage   struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	headers := map[string]string{"x-api-key": c.profile.APIKey, "anthropic-version": "2023-06-01"}
	if err := c.do(ctx, "/messages", body, headers, &payload); err != nil {
		return Completion{}, err
	}
	var b strings.Builder
	for _, block := range payload.Content {
		b.WriteString(block.Text)
	}
	return Completion{Text: b.String(), Usage: Usage{InputTokens: payload.Usage.Input, OutputTokens: payload.Usage.Output}}, nil
}

func (c *anthropicClient) Stream(ctx context.Context, prompt string, maxTokens int, onDelta func(string) error) (Completion, error) {
	body := map[string]any{"model": c.profile.Model, "max_tokens": maxTokens, "stream": true, "messages": []map[string]string{{"role": "user", "content": prompt}}}
	headers := map[string]string{"x-api-key": c.profile.APIKey, "anthropic-version": "2023-06-01"}
	var result Completion
	err := c.stream(ctx, "/messages", body, headers, func(data []byte) error {
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					Input int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if event.Delta.Text != "" {
			result.Text += event.Delta.Text
			return onDelta(event.Delta.Text)
		}
		if event.Type == "message_start" {
			result.Usage.InputTokens = event.Message.Usage.Input
		}
		if event.Type == "message_delta" {
			result.Usage.OutputTokens = event.Usage.Output
		}
		return nil
	})
	return result, err
}

func (c *httpClient) do(ctx context.Context, endpoint string, body any, headers map[string]string, target any) error {
	if strings.TrimSpace(c.profile.APIKey) == "" {
		return fmt.Errorf("provider %s API key is not configured", c.profile.Name)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.profile.BaseURL, "/") + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("provider request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, redact(string(limited), c.profile.APIKey))
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return decodeSSE(resp.Body, target)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(target)
}

func (c *httpClient) stream(ctx context.Context, endpoint string, body any, headers map[string]string, handle func([]byte) error) error {
	if strings.TrimSpace(c.profile.APIKey) == "" {
		return fmt.Errorf("provider %s API key is not configured", c.profile.Name)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.profile.BaseURL, "/") + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("provider request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, redact(string(limited), c.profile.APIKey))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := bytes.TrimSpace([]byte(strings.TrimPrefix(line, "data:")))
		if string(data) == "[DONE]" {
			continue
		}
		if err := handle(data); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func decodeSSE(r io.Reader, target any) error {
	var last []byte
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "data:") {
			data := bytes.TrimSpace([]byte(strings.TrimPrefix(line, "data:")))
			if string(data) != "[DONE]" {
				last = append(last[:0], data...)
			}
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	if len(last) == 0 {
		return fmt.Errorf("empty provider stream")
	}
	return json.Unmarshal(last, target)
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}
