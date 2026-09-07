package protocol

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/Lundomn/gopherharness/internal/model"
)

type Kind string

const (
	KindTools Kind = "tools"
	KindFinal Kind = "final"
	KindRetry Kind = "retry"
)

type Parsed struct {
	Kind  Kind
	Tools []model.ToolCall
	Text  string
}

var (
	toolRE  = regexp.MustCompile(`(?s)<tool\b([^>]*)>(.*?)</tool>`)
	finalRE = regexp.MustCompile(`(?s)<final>(.*?)</final>`)
	attrRE  = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)="(.*?)"`)
)

func Parse(raw string) Parsed {
	toolAt, finalAt := strings.Index(raw, "<tool"), strings.Index(raw, "<final>")
	if toolAt >= 0 && (finalAt < 0 || toolAt < finalAt) {
		calls, err := parseTools(raw)
		if err != nil {
			return Parsed{Kind: KindRetry, Text: RetryNotice(err.Error())}
		}
		if len(calls) > 0 {
			return Parsed{Kind: KindTools, Tools: calls}
		}
	}
	if m := finalRE.FindStringSubmatch(raw); len(m) == 2 {
		return Parsed{Kind: KindFinal, Text: strings.TrimSpace(m[1])}
	}
	problem := "missing <tool> or <final> tag"
	if strings.TrimSpace(raw) == "" {
		problem = "empty response"
	}
	return Parsed{Kind: KindRetry, Text: RetryNotice(problem)}
}

func RetryNotice(problem string) string {
	return fmt.Sprintf("Your previous response could not be executed. Problem: %s. Return one or more valid <tool> calls, or one <final> answer.", problem)
}

func parseTools(raw string) ([]model.ToolCall, error) {
	matches := toolRE.FindAllStringSubmatch(raw, -1)
	var calls []model.ToolCall
	var firstErr error
	for _, m := range matches {
		attrs := parseAttrs(m[1])
		body := strings.TrimSpace(m[2])
		if name := strings.TrimSpace(attrs["name"]); name != "" {
			args := map[string]any{}
			for k, v := range attrs {
				if k != "name" {
					args[k] = html.UnescapeString(v)
				}
			}
			for _, tag := range []string{"content", "old_text", "new_text"} {
				if v, ok := extractRaw(m[2], tag); ok {
					args[tag] = html.UnescapeString(v)
				}
			}
			if name == "write_file" {
				if _, ok := args["content"]; !ok && body != "" {
					args["content"] = body
				}
			}
			calls = append(calls, model.ToolCall{Name: name, Args: args})
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("tool payload must be valid JSON or supported XML")
			}
			continue
		}
		normalized, err := normalizePayload(payload)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		calls = append(calls, normalized...)
	}
	if len(calls) > 0 {
		return calls, nil
	}
	return nil, firstErr
}

func normalizePayload(payload any) ([]model.ToolCall, error) {
	if list, ok := payload.([]any); ok {
		if len(list) == 0 {
			return nil, fmt.Errorf("tool JSON list must not be empty")
		}
		var calls []model.ToolCall
		for _, item := range list {
			nested, err := normalizePayload(item)
			if err != nil {
				return nil, err
			}
			calls = append(calls, nested...)
		}
		return calls, nil
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool JSON must be an object with name and args")
	}
	name, ok := object["name"].(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("tool JSON must be an object with name and args")
	}
	args := map[string]any{}
	if rawArgs, exists := object["args"]; exists {
		var argsOK bool
		args, argsOK = rawArgs.(map[string]any)
		if !argsOK {
			return nil, fmt.Errorf("tool args must be an object")
		}
	} else if rawArgs, exists := object["arguments"]; exists {
		var argsOK bool
		args, argsOK = rawArgs.(map[string]any)
		if !argsOK {
			return nil, fmt.Errorf("tool args must be an object")
		}
	}
	return []model.ToolCall{{Name: name, Args: args}}, nil
}

func parseAttrs(raw string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRE.FindAllStringSubmatch(raw, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func extractRaw(raw, tag string) (string, bool) {
	re := regexp.MustCompile(`(?s)<` + regexp.QuoteMeta(tag) + `>(.*?)</` + regexp.QuoteMeta(tag) + `>`)
	m := re.FindStringSubmatch(raw)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}
