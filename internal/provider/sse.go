package provider

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

const maxStreamBytes = 32 << 20
const maxEventBytes = 4 << 20

// Events end at a blank line; multiple data fields form one payload. Never
// expose an unterminated event as a complete tool call. Limits also count
// ignored fields/comments so a bad peer cannot consume unbounded resources.
func readEventStream(r io.Reader, handle func([]byte) error) error {
	limited := &io.LimitedReader{R: r, N: maxStreamBytes + 1}
	s := bufio.NewScanner(limited)
	s.Buffer(make([]byte, 64<<10), maxEventBytes)
	s.Split(eventLine)
	var payload []byte
	first := true
	for s.Scan() {
		line := s.Bytes()
		if first {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
			first = false
		}
		if limited.N == 0 {
			return fmt.Errorf("provider stream exceeds %d bytes", maxStreamBytes)
		}
		if len(line) == 0 {
			if len(payload) > 0 {
				data := payload[:len(payload)-1]
				if !bytes.Equal(data, []byte("[DONE]")) {
					if err := handle(data); err != nil {
						return err
					}
				}
				payload = payload[:0]
			}
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		if !bytes.Equal(field, []byte("data")) {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		if len(payload)+len(value)+1 > maxEventBytes {
			return fmt.Errorf("provider event exceeds %d bytes", maxEventBytes)
		}
		payload = append(payload, value...)
		payload = append(payload, '\n')
	}
	if err := s.Err(); err != nil {
		return err
	}
	if limited.N == 0 {
		return fmt.Errorf("provider stream exceeds %d bytes", maxStreamBytes)
	}
	if len(payload) > 0 {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func eventLine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
		if b == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			n := i + 1
			if n < len(data) && data[n] == '\n' {
				n++
			}
			return n, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
