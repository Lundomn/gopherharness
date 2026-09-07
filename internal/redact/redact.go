package redact

import (
	"regexp"
	"strings"
)

const Replacement = "[REDACTED]"

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)sk-[a-z0-9_-]{12,}`),
	regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`(?i)(api[_ -]?key\s*[:=]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(password\s*[:=]\s*)[^\s,;]+`),
}

type Redactor struct{ secrets []string }

func New(secrets ...string) *Redactor {
	seen := map[string]bool{}
	clean := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) >= 4 && !seen[secret] {
			seen[secret] = true
			clean = append(clean, secret)
		}
	}
	return &Redactor{secrets: clean}
}

func (r *Redactor) Text(value string) string {
	for _, secret := range r.secrets {
		value = strings.ReplaceAll(value, secret, Replacement)
	}
	for _, pattern := range secretPatterns {
		value = pattern.ReplaceAllString(value, Replacement)
	}
	return value
}

func (r *Redactor) Value(value any) any {
	return r.value("", value)
}

func (r *Redactor) value(key string, value any) any {
	if sensitiveKey(key) && value != nil {
		return Replacement
	}
	switch typed := value.(type) {
	case string:
		return r.Text(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, child := range typed {
			out[childKey] = r.value(childKey, child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, child := range typed {
			out[index] = r.value(key, child)
		}
		return out
	case []string:
		out := make([]string, len(typed))
		for index, child := range typed {
			out[index] = r.Text(child)
		}
		return out
	default:
		return value
	}
}

func (r *Redactor) Map(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	redacted, ok := r.Value(value).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return redacted
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, marker := range []string{"api_key", "apikey", "authorization", "password", "passwd", "secret", "access_token", "refresh_token", "cookie"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
