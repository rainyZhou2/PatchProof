// Package harness implements the generated Go harness used by the
// go-differential engine together with the process-isolation helpers shared with
// the go-native-fuzz engine.
//
// The harness is a small Go program that is generated into every snapshot. It
// exposes a stable JSON-line protocol:
//
//   - "-describe" prints one JSON object describing the target function's
//     parameter and result types.
//   - "-single" reads one JSON array of arguments from stdin and prints one
//     observation.
//   - "-batch" reads newline-delimited JSON arrays from stdin and prints one
//     observation per line.
//
// Observations always have the shape
//
//	{"status":"return|panic|invalid","value":[...],"panic":"<type: message>","elapsed_ms":N}
//
// The elapsed_ms field is timing metadata and is excluded from canonical
// observations used for equality checks and evidence.
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patchproof/patchproof/pkg/evidence"
)

// ProtocolVersion identifies the harness observation protocol.
const ProtocolVersion = "patchproof.godifferential.harness/v1"

// Observation status values.
const (
	StatusReturn  = "return"
	StatusPanic   = "panic"
	StatusInvalid = "invalid"
)

// MaxInputBytes bounds a single case's argument JSON.
const MaxInputBytes = 1 << 20

// MaxObservationBytes bounds a single canonical observation.
const MaxObservationBytes = 1 << 20

// TypeDesc is the harness' description of one Go type. It is intentionally
// limited to the information needed for JSON-directed seed generation.
type TypeDesc struct {
	Kind string    `json:"kind"`
	Name string    `json:"name"`
	Bits int       `json:"bits,omitempty"`
	Elem *TypeDesc `json:"elem,omitempty"`
	Key  *TypeDesc `json:"key,omitempty"`
}

// Signature describes the target function.
type Signature struct {
	Protocol string     `json:"protocol"`
	Package  string     `json:"package"`
	Function string     `json:"function"`
	Arity    int        `json:"arity"`
	Variadic bool       `json:"variadic"`
	Params   []TypeDesc `json:"params"`
	Results  []TypeDesc `json:"results"`
}

// Observation is one harness result.
type Observation struct {
	Status    string          `json:"status"`
	Value     json.RawMessage `json:"value"`
	Panic     string          `json:"panic"`
	ElapsedMS int64           `json:"elapsed_ms"`
}

// CanonicalObservation is the deterministic part of an observation. Timing is
// intentionally omitted so that CAS digests are stable across runs and hosts.
type CanonicalObservation struct {
	Status string          `json:"status"`
	Value  json.RawMessage `json:"value"`
	Panic  string          `json:"panic"`
}

// Canonical returns the canonical JSON encoding of the observation without
// timing metadata.
func (o Observation) Canonical() ([]byte, error) {
	value := o.Value
	if len(value) == 0 {
		value = json.RawMessage("[]")
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode observation value: %w", err)
	}
	return evidence.Canonicalize(CanonicalObservation{
		Status: o.Status,
		Value:  json.RawMessage(mustMarshal(sanitizeNegativeZero(decoded))),
		Panic:  o.Panic,
	})
}

// sanitizeNegativeZero replaces -0 with 0. Canonical JSON (RFC 8785) serializes
// negative zero as 0, and the evidence canonicalizer rejects the raw "-0" form.
func sanitizeNegativeZero(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if typed.String() == "-0" {
			return json.Number("0")
		}
		return typed
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = sanitizeNegativeZero(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = sanitizeNegativeZero(item)
		}
		return out
	default:
		return value
	}
}

func mustMarshal(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return []byte("[]")
	}
	return raw
}

// ParseObservations decodes newline-delimited observations. When want is
// non-negative the number of observations must match exactly; a mismatch means
// the harness process terminated early and the batch must be treated as an
// infrastructure failure rather than compared.
func ParseObservations(stdout []byte, want int) ([]Observation, error) {
	if len(stdout) == 0 {
		return nil, fmt.Errorf("harness produced no output")
	}
	lines := bytes.Split(stdout, []byte("\n"))
	out := make([]Observation, 0, len(lines))
	for index, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var observation Observation
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&observation); err != nil {
			return nil, fmt.Errorf("decode observation %d: %w", index+1, err)
		}
		switch observation.Status {
		case StatusReturn, StatusPanic, StatusInvalid:
		default:
			return nil, fmt.Errorf("observation %d has unsupported status %q", index+1, observation.Status)
		}
		if len(observation.Value) == 0 {
			observation.Value = json.RawMessage("[]")
		}
		if !json.Valid(observation.Value) {
			return nil, fmt.Errorf("observation %d has invalid value JSON", index+1)
		}
		out = append(out, observation)
	}
	if want >= 0 && len(out) != want {
		return nil, fmt.Errorf("harness produced %d observations, want %d", len(out), want)
	}
	return out, nil
}

// ParseSignature decodes a -describe payload.
func ParseSignature(stdout []byte) (Signature, error) {
	var signature Signature
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(stdout)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&signature); err != nil {
		return Signature{}, fmt.Errorf("decode signature: %w", err)
	}
	if signature.Protocol != ProtocolVersion {
		return Signature{}, fmt.Errorf("harness protocol %q is not supported", signature.Protocol)
	}
	if signature.Arity != len(signature.Params) {
		return Signature{}, fmt.Errorf("harness signature arity %d does not match %d parameters", signature.Arity, len(signature.Params))
	}
	if signature.Arity < 0 {
		return Signature{}, fmt.Errorf("harness signature arity must not be negative")
	}
	if signature.Variadic {
		return Signature{}, fmt.Errorf("variadic target functions are not supported")
	}
	if signature.Arity > 32 {
		return Signature{}, fmt.Errorf("target function has %d parameters; at most 32 are supported", signature.Arity)
	}
	return signature, nil
}

// EncodeBatch serializes canonical argument arrays as NDJSON.
func EncodeBatch(cases [][]byte) []byte {
	size := 0
	for _, item := range cases {
		size += len(item) + 1
	}
	buf := make([]byte, 0, size)
	for _, item := range cases {
		buf = append(buf, item...)
		buf = append(buf, '\n')
	}
	return buf
}

// LooksLikeObservation reports whether raw starts with a JSON object, used to
// describe malformed harness output without panicking.
func LooksLikeObservation(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// TruncateForLog bounds a diagnostic string so that logs cannot grow without
// limit when the target writes to stdout/stderr itself.
func TruncateForLog(raw []byte, limit int) string {
	if limit <= 0 {
		limit = 4096
	}
	text := strings.ToValidUTF8(string(raw), "?")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
