package governance

import "strings"

// This intentionally recognizes only one simple top-level command. Pipelines,
// substitutions, comments and command lists can hide failure (test || true),
// so they are never verification evidence. Recognizing a check and exit code
// proves that command completed, not that its test suite is comprehensive.
func verificationWords(command string) ([]string, bool) {
	var words []string
	var word strings.Builder
	var quote rune
	started := false
	for _, ch := range command {
		if ch == '\n' || ch == '\r' || ch == 0 {
			return nil, false
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				if quote == '"' && (ch == '$' || ch == '`' || ch == '\\') {
					return nil, false
				}
				word.WriteRune(ch)
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote, started = ch, true
		case ' ', '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		case ';', '&', '|', '<', '>', '(', ')', '{', '}', '$', '`', '\\', '#':
			return nil, false
		default:
			word.WriteRune(ch)
			started = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		words = append(words, word.String())
	}
	return words, true
}

func environmentAssignment(word string) bool {
	key, _, ok := strings.Cut(word, "=")
	if !ok || key == "" {
		return false
	}
	for i, c := range key {
		if c != '_' && !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
