package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
)

type CommandConfig struct {
	Command       []string
	Env           []string
	Dir           string
	Stderr        io.Writer
	MaxFrameBytes int
	StartTimeout  time.Duration
	ShutdownGrace time.Duration
}

type Client struct {
	mu       sync.Mutex
	conn     io.ReadWriteCloser
	closed   bool
	maxFrame int
	cmd      *exec.Cmd
	grace    time.Duration
}

func Dial(command []string, config CommandConfig) (*Client, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("plugin: command is required")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = config.Env
	if len(config.Env) == 0 {
		cmd.Env = os.Environ()
	}
	cmd.Dir = config.Dir
	if config.Stderr != nil {
		cmd.Stderr = config.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: start %q: %w", command[0], err)
	}
	grace := config.ShutdownGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	return &Client{conn: &pipeConn{reader: bufio.NewReader(stdout), writer: stdin, stdout: stdout, stdin: stdin}, cmd: cmd, maxFrame: config.MaxFrameBytes, grace: grace}, nil
}

func NewClient(connection io.ReadWriteCloser, maxFrameBytes int) *Client {
	if maxFrameBytes <= 0 {
		maxFrameBytes = DefaultMaxFrameBytes
	}
	return &Client{conn: connection, maxFrame: maxFrameBytes, grace: 5 * time.Second}
}

func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var capabilities Capabilities
	if err := c.call(ctx, FrameCapabilities, nil, &capabilities); err != nil {
		return Capabilities{}, err
	}
	if capabilities.ProtocolMajor != ProtocolMajor {
		return Capabilities{}, fmt.Errorf("plugin: incompatible protocol major %d (required %d)", capabilities.ProtocolMajor, ProtocolMajor)
	}
	return capabilities, nil
}

func (c *Client) Execute(ctx context.Context, request StepRequest) (StepResponse, error) {
	var response StepResponse
	if err := c.call(ctx, FrameExecute, request, &response); err != nil {
		return StepResponse{}, err
	}
	if response.AttemptID != "" && response.AttemptID != request.AttemptID {
		return StepResponse{}, fmt.Errorf("plugin: response attempt %q does not match %q", response.AttemptID, request.AttemptID)
	}
	if response.FencingGeneration != 0 && response.FencingGeneration != request.FencingGeneration {
		return StepResponse{}, fmt.Errorf("plugin: response fencing generation does not match")
	}
	return response, nil
}

func (c *Client) call(ctx context.Context, frameType FrameType, payload any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	id := uuid.NewString()
	var encoded json.RawMessage
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		encoded = body
	}
	if err := WriteFrame(c.conn, Frame{ID: id, Type: frameType, Payload: encoded}, c.maxFrame); err != nil {
		return err
	}
	done := make(chan struct{})
	watcher := make(chan struct{})
	go func() {
		defer close(watcher)
		select {
		case <-ctx.Done():
			_ = c.conn.Close()
		case <-done:
		}
	}()
	frame, err := ReadFrame(c.conn, c.maxFrame)
	close(done)
	// Wait for the watcher to observe done before returning. A caller that
	// cancels its context immediately after a successful call would otherwise
	// race the watcher and close a healthy connection.
	<-watcher
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if frame.ID != "" && frame.ID != id {
		return fmt.Errorf("%w: response id %q does not match %q", ErrProtocol, frame.ID, id)
	}
	if frame.Type == FrameError && frame.Error != nil {
		return frame.Error
	}
	if frame.Type != FrameResponse {
		return fmt.Errorf("%w: expected response, got %q", ErrProtocol, frame.Type)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(frame.Payload, out)
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	connection := c.conn
	cmd := c.cmd
	c.mu.Unlock()
	_ = WriteFrame(connection, Frame{Type: FrameShutdown}, c.maxFrame)
	_ = connection.Close()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		return err
	case <-time.After(c.grace):
		_ = cmd.Process.Kill()
		return <-waitCh
	}
}

type pipeConn struct {
	reader *bufio.Reader
	writer io.Writer
	stdout io.ReadCloser
	stdin  io.WriteCloser
}

func (p *pipeConn) Read(data []byte) (int, error)  { return p.reader.Read(data) }
func (p *pipeConn) Write(data []byte) (int, error) { return p.writer.Write(data) }
func (p *pipeConn) Close() error {
	_ = p.stdin.Close()
	return p.stdout.Close()
}

var _ io.ReadWriteCloser = (*pipeConn)(nil)
