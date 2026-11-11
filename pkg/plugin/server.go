package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type Service interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	Execute(ctx context.Context, request StepRequest) (StepResponse, error)
}

type Server struct {
	Service       Service
	MaxFrameBytes int
	// ExecuteTimeout bounds one Execute call when the request carries no
	// deadline. Zero means no additional timeout beyond the caller's context.
	ExecuteTimeout time.Duration
}

func (s *Server) Serve(ctx context.Context, connection io.ReadWriteCloser) error {
	if s.Service == nil {
		return fmt.Errorf("plugin server: service is required")
	}
	defer connection.Close()
	maxFrame := s.MaxFrameBytes
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrameBytes
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		frame, err := ReadFrame(connection, maxFrame)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch frame.Type {
		case FrameCapabilities:
			capabilities, err := s.Service.Capabilities(ctx)
			if err != nil {
				if writeErr := respondError(connection, frame.ID, "CAPABILITIES_FAILED", err, maxFrame); writeErr != nil {
					return writeErr
				}
				continue
			}
			if capabilities.ProtocolMajor == 0 {
				capabilities.ProtocolMajor = ProtocolMajor
			}
			if capabilities.ProtocolMinor == 0 {
				capabilities.ProtocolMinor = ProtocolMinor
			}
			if err := respond(connection, frame.ID, capabilities, maxFrame); err != nil {
				return err
			}
		case FrameExecute:
			var request StepRequest
			if err := json.Unmarshal(frame.Payload, &request); err != nil {
				if writeErr := respondError(connection, frame.ID, "INVALID_REQUEST", err, maxFrame); writeErr != nil {
					return writeErr
				}
				continue
			}
			executeCtx, cancel := s.executionContext(ctx, request)
			response, err := s.Service.Execute(executeCtx, request)
			cancel()
			if err != nil {
				if writeErr := respondError(connection, frame.ID, "EXECUTION_FAILED", err, maxFrame); writeErr != nil {
					return writeErr
				}
				continue
			}
			if err := respond(connection, frame.ID, response, maxFrame); err != nil {
				return err
			}
		case FrameShutdown:
			_ = respond(connection, frame.ID, map[string]string{"status": "stopping"}, maxFrame)
			return nil
		default:
			if err := respondError(connection, frame.ID, "UNKNOWN_FRAME", fmt.Errorf("unknown frame type %q", frame.Type), maxFrame); err != nil {
				return err
			}
		}
	}
}

func (s *Server) executionContext(ctx context.Context, request StepRequest) (context.Context, context.CancelFunc) {
	if !request.Deadline.IsZero() {
		return context.WithDeadline(ctx, request.Deadline)
	}
	if s.ExecuteTimeout > 0 {
		return context.WithTimeout(ctx, s.ExecuteTimeout)
	}
	return context.WithCancel(ctx)
}

func respond(w io.Writer, id string, payload any, maxFrame int) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return WriteFrame(w, Frame{ID: id, Type: FrameResponse, Payload: encoded}, maxFrame)
}

func respondError(w io.Writer, id, code string, cause error, maxFrame int) error {
	return WriteFrame(w, Frame{ID: id, Type: FrameError, Error: &RemoteError{Code: code, Message: cause.Error()}}, maxFrame)
}
