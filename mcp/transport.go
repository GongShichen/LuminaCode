package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

type McpTransport interface {
	Connect(ctx context.Context) error
	Send(ctx context.Context, message any) error
	Receive(ctx context.Context) (any, error)
	Close(ctx context.Context) error
	IsConnected() bool
}

type StdioTransport struct {
	Command string
	Args    []string
	Env     map[string]string
	CWD     string

	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	stderrDone chan struct{}
	mu         sync.Mutex
	buffer     string
}

func NewStdioTransport(command string, args []string, env map[string]string, cwd string) *StdioTransport {
	return &StdioTransport{Command: command, Args: args, Env: env, CWD: cwd}
}

func (t *StdioTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command(t.Command, t.Args...)
	cmd.Env = t.safeEnv()
	cmd.Dir = t.CWD
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	t.cmd = cmd
	t.stdin = stdin
	t.stdout = bufio.NewReader(stdout)
	t.stderrDone = make(chan struct{})
	go t.readStderr(stderr)
	t.buffer = ""
	return nil
}

func (t *StdioTransport) Send(_ context.Context, message any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stdin == nil {
		return fmt.Errorf("StdioTransport not connected")
	}
	_, err := t.stdin.Write([]byte(SerializeMessage(message) + "\n"))
	return err
}

func (t *StdioTransport) Receive(ctx context.Context) (any, error) {
	t.mu.Lock()
	reader := t.stdout
	t.mu.Unlock()
	if reader == nil {
		return nil, nil
	}
	type result struct {
		msg any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					t.mu.Lock()
					if t.stdout == reader {
						t.cmd = nil
					}
					t.mu.Unlock()
					ch <- result{}
				} else {
					ch <- result{err: err}
				}
				return
			}
			line = strings.ToValidUTF8(line, "\uFFFD")
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			ch <- result{msg: ParseMessage(line)}
			return
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res.msg, res.err
	}
}

func (t *StdioTransport) Close(ctx context.Context) error {
	t.mu.Lock()
	cmd := t.cmd
	stdin := t.stdin
	t.cmd = nil
	t.stdin = nil
	t.stdout = nil
	t.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		return nil
	}
	defer func() {
		if stdin != nil {
			_ = stdin.Close()
		}
	}()
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		_ = terminateProcess(cmd.Process)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return ctx.Err()
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			return nil
		case err := <-done:
			_ = err
			return nil
		}
	}
	return nil
}

func (t *StdioTransport) IsConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cmd != nil && t.cmd.ProcessState == nil
}

func (t *StdioTransport) safeEnv() []string {
	safeKeys := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "USERNAME": true, "USERPROFILE": true,
		"LANG": true, "LC_ALL": true, "TMPDIR": true, "TEMP": true, "TMP": true,
	}
	if runtime.GOOS == "windows" {
		safeKeys["SYSTEMROOT"] = true
	} else {
		safeKeys["SHELL"] = true
	}
	var env []string
	for _, item := range os.Environ() {
		key := strings.SplitN(item, "=", 2)[0]
		if safeKeys[key] {
			env = append(env, item)
		}
	}
	for key, value := range t.Env {
		env = append(env, key+"="+value)
	}
	return env
}

func (t *StdioTransport) readStderr(stderr io.Reader) {
	defer close(t.stderrDone)
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		_ = scanner.Text()
	}
}

type HTTPTransport struct {
	URL             string
	Headers         map[string]string
	Client          *http.Client
	pendingResponse any
	connected       bool
	mu              sync.Mutex
}

func NewHTTPTransport(url string, headers map[string]string) *HTTPTransport {
	return &HTTPTransport{URL: strings.TrimRight(url, "/"), Headers: headers, Client: &http.Client{Timeout: 30 * time.Second}}
}

func (t *HTTPTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	t.connected = true
	t.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, t.URL, nil)
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		for key, value := range t.Headers {
			req.Header.Set(key, value)
		}
		if resp, err := t.Client.Do(req); err == nil && resp != nil {
			_ = resp.Body.Close()
		}
	}
	return nil
}

func (t *HTTPTransport) Send(ctx context.Context, message any) error {
	t.mu.Lock()
	connected := t.connected
	t.mu.Unlock()
	if !connected {
		return fmt.Errorf("HttpTransport not connected")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewBufferString(SerializeMessage(message)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, value := range t.Headers {
		req.Header.Set(key, value)
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed any
	if len(strings.TrimSpace(string(body))) > 0 {
		switch msg := ParseMessage(string(body)).(type) {
		case JSONRPCResponse, JSONRPCError:
			parsed = msg
		}
	}
	t.mu.Lock()
	t.pendingResponse = parsed
	t.mu.Unlock()
	return nil
}

func (t *HTTPTransport) Receive(context.Context) (any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	resp := t.pendingResponse
	t.pendingResponse = nil
	return resp, nil
}

func (t *HTTPTransport) Close(context.Context) error {
	t.mu.Lock()
	t.connected = false
	t.pendingResponse = nil
	t.mu.Unlock()
	return nil
}

func (t *HTTPTransport) IsConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connected
}
