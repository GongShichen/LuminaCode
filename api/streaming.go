package api

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

func ParseSSEStream(r io.Reader) (<-chan map[string]any, <-chan error) {
	out := make(chan map[string]any)
	errCh := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errCh)

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			dataStr := line[len("data: "):]
			if dataStr == "[DONE]" {
				return
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(dataStr), &obj); err != nil {
				continue
			}

			out <- obj
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
		}
	}()
	return out, errCh
}
