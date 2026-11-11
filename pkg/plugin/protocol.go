// Package plugin implements the reference stdio plugin protocol for PatchProof
// analysis and verification engines. The gRPC contract in api/proto mirrors
// these messages; generated gRPC bindings are not required to use this package.
package plugin

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	ProtocolMajor = 1
	// ProtocolMinor is bumped when additive fields are introduced. Clients and
	// servers must accept any minor version within a matching major version.
	ProtocolMinor = 1

	DefaultMaxFrameBytes = 16 << 20
)

type FrameType string

const (
	FrameCapabilities FrameType = "capabilities"
	FrameExecute      FrameType = "execute"
	FrameShutdown     FrameType = "shutdown"
	FrameResponse     FrameType = "response"
	FrameError        FrameType = "error"
)

type CASRef struct {
	Algorithm   string `json:"algorithm"`
	Digest      string `json:"digest"`
	SizeBytes   int64  `json:"size_bytes"`
	TenantScope string `json:"tenant_scope,omitempty"`
}

// StagedInput describes an input that the platform already materialized inside
// the worker sandbox. Path is an absolute path to a file or directory the
// plugin may read; Ref is the CAS identity the path was materialized from.
// Sandbox-native plugins must use StagedInputs instead of downloading Inputs.
type StagedInput struct {
	Ref  CASRef `json:"ref"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// FileArtifact names an output file produced by the plugin. Path is relative to
// StepRequest.OutputDir; absolute paths, paths escaping OutputDir and symbolic
// links are rejected by the host. The host, not the plugin, uploads the file to
// tenant-scoped CAS.
type FileArtifact struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"media_type,omitempty"`
}

// EvidenceScope mirrors domain.EvidenceScope on the wire.
type EvidenceScope struct {
	InputDomain    string            `json:"input_domain"`
	SeedCount      int64             `json:"seed_count,omitempty"`
	GeneratedCases int64             `json:"generated_cases,omitempty"`
	ElapsedMS      int64             `json:"elapsed_ms,omitempty"`
	Budget         string            `json:"budget,omitempty"`
	Details        map[string]string `json:"details,omitempty"`
}

// EvidenceDraft is an evidence envelope proposed by a plugin. The host owns
// tenant, run, step, identity, engine and environment metadata, and resolves
// ArtifactNames/InputNames to CAS references before validating the result.
type EvidenceDraft struct {
	ClaimID       string        `json:"claim_id"`
	Kind          string        `json:"kind"`
	Result        string        `json:"result"`
	Scope         EvidenceScope `json:"scope"`
	Assumptions   []string      `json:"assumptions,omitempty"`
	Limits        []string      `json:"limits,omitempty"`
	ArtifactNames []string      `json:"artifact_names,omitempty"`
	InputNames    []string      `json:"input_names,omitempty"`
}

// CounterexampleDraft is a counterexample proposed by a plugin. The three file
// fields name entries of StepResponse.Files; the host resolves them to CAS
// references and fills in tenant, run, claim and timestamp metadata.
type CounterexampleDraft struct {
	CaseID              string   `json:"case_id"`
	ClaimID             string   `json:"claim_id"`
	Scope               string   `json:"scope"`
	InputCodec          string   `json:"input_codec"`
	InputFile           string   `json:"input_file"`
	BaseObservationFile string   `json:"base_observation_file"`
	HeadObservationFile string   `json:"head_observation_file"`
	Reproduced          bool     `json:"reproduced"`
	Minimized           bool     `json:"minimized"`
	Nondeterminism      []string `json:"nondeterminism,omitempty"`
}

type Capabilities struct {
	PluginID      string   `json:"plugin_id"`
	Version       string   `json:"version"`
	ProtocolMajor uint32   `json:"protocol_major"`
	ProtocolMinor uint32   `json:"protocol_minor"`
	Languages     []string `json:"languages,omitempty"`
	StepKinds     []string `json:"step_kinds,omitempty"`
	Architectures []string `json:"architectures,omitempty"`
}

type StepRequest struct {
	RunID             string `json:"run_id"`
	StepID            string `json:"step_id"`
	AttemptID         string `json:"attempt_id"`
	FencingGeneration uint64 `json:"fencing_generation"`
	PolicyDigest      string `json:"policy_digest"`
	StepKind          string `json:"step_kind"`
	Language          string `json:"language"`
	Engine            string `json:"engine"`
	// Inputs is the remote-CAS form: the plugin downloads refs itself. It stays
	// populated for plugins that are not sandbox-native.
	Inputs map[string]CASRef `json:"inputs,omitempty"`
	// Workspace is the worker sandbox root. OutputDir is a host-created,
	// writable directory inside Workspace; plugin output files are relative to
	// it. StagedInputs maps input names to paths already materialized under
	// Workspace.
	Workspace    string                 `json:"workspace,omitempty"`
	OutputDir    string                 `json:"output_dir,omitempty"`
	StagedInputs map[string]StagedInput `json:"staged_inputs,omitempty"`
	Options      map[string]string      `json:"options,omitempty"`
	Deadline     time.Time              `json:"deadline"`
}

// StepMetrics reports resource usage measured by the plugin.
type StepMetrics struct {
	CPUSeconds              float64 `json:"cpu_seconds"`
	MemoryGiBSeconds        float64 `json:"memory_gib_seconds"`
	EphemeralDiskGiBSeconds float64 `json:"ephemeral_disk_gib_seconds"`
	EgressBytes             int64   `json:"egress_bytes"`
	CASWrittenBytes         int64   `json:"cas_written_bytes"`
	QueueWaitMS             int64   `json:"queue_wait_ms"`
	ExecutionMS             int64   `json:"execution_ms"`
	UploadMS                int64   `json:"upload_ms"`
}

type StepResponse struct {
	AttemptID         string `json:"attempt_id"`
	FencingGeneration uint64 `json:"fencing_generation"`
	Outcome           string `json:"outcome"`
	ErrorClass        string `json:"error_class,omitempty"`
	Message           string `json:"message,omitempty"`
	Retryable         bool   `json:"retryable"`
	ResultDigest      string `json:"result_digest,omitempty"`
	// Artifacts is the remote-CAS form: refs the host records without
	// uploading anything.
	Artifacts []CASRef `json:"artifacts,omitempty"`
	// Files are sandbox output files the host uploads into tenant-scoped CAS.
	Files []FileArtifact `json:"files,omitempty"`
	// EvidenceDrafts and CounterexampleDrafts are validated and completed by
	// the host before they become domain objects.
	EvidenceDrafts       []EvidenceDraft       `json:"evidence_drafts,omitempty"`
	CounterexampleDrafts []CounterexampleDraft `json:"counterexample_drafts,omitempty"`
	// Evidence and Counterexamples are the legacy raw-JSON form used by remote
	// plugins; sandbox-native plugins should return drafts instead.
	Evidence        []json.RawMessage `json:"evidence,omitempty"`
	Counterexamples []json.RawMessage `json:"counterexamples,omitempty"`
	Metrics         *StepMetrics      `json:"metrics,omitempty"`
	Outputs         map[string]string `json:"outputs,omitempty"`
}

type Frame struct {
	ID      string          `json:"id,omitempty"`
	Type    FrameType       `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *RemoteError    `json:"error,omitempty"`
}

type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("plugin error %s: %s", e.Code, e.Message)
}

var (
	ErrFrameTooLarge = errors.New("plugin frame exceeds the size limit")
	ErrClosed        = errors.New("plugin connection is closed")
	ErrProtocol      = errors.New("plugin protocol violation")
)

func WriteFrame(w io.Writer, frame Frame, maxBytes int) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if maxBytes > 0 && len(payload) > maxBytes {
		return ErrFrameTooLarge
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func ReadFrame(r io.Reader, maxBytes int) (Frame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return Frame{}, fmt.Errorf("%w: empty frame", ErrProtocol)
	}
	if maxBytes > 0 && int(length) > maxBytes {
		return Frame{}, ErrFrameTooLarge
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	var frame Frame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	if frame.Type == "" {
		return Frame{}, fmt.Errorf("%w: missing frame type", ErrProtocol)
	}
	return frame, nil
}

func (c Capabilities) Compatible() bool {
	return c.ProtocolMajor == ProtocolMajor
}
