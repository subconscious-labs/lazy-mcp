package broker

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// sseScanner reads complete Server-Sent Events. Per the SSE spec, an
// event is a group of lines terminated by a blank line; multiple
// `data:` lines within one event are joined with '\n'. This is what
// MCP streamable-HTTP uses to wrap responses and notifications.
type sseScanner struct {
	sc       *bufio.Scanner
	maxBytes int
}

func newSSEScanner(r io.Reader, maxBytes int) *sseScanner {
	sc := bufio.NewScanner(r)
	// Individual lines and total event payload are bounded by maxBytes.
	sc.Buffer(make([]byte, 0, 64*1024), maxBytes)
	return &sseScanner{sc: sc, maxBytes: maxBytes}
}

// next returns the next event's data payload. io.EOF means the stream
// ended; any other error means the event exceeded maxBytes or the
// underlying reader failed.
func (s *sseScanner) next() ([]byte, error) {
	var (
		data  []byte
		total int
	)
	for s.sc.Scan() {
		line := s.sc.Bytes()
		if len(line) == 0 {
			if data != nil {
				return data, nil
			}
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // ignore event:, id:, retry:, comments
		}
		payload := line[len("data:"):]
		if len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}
		if data != nil {
			data = append(data, '\n')
			total++
		}
		data = append(data, payload...)
		total += len(payload)
		if total > s.maxBytes {
			return nil, fmt.Errorf("sse event exceeds %d bytes", s.maxBytes)
		}
	}
	if err := s.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}
