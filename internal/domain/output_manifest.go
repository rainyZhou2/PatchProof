package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OutputManifest is the durable record of a step's named outputs and
// content-addressed artifacts. It is stored in steps.output_manifest so that
// downstream steps can resolve dependency outputs without re-reading results.
type OutputManifest struct {
	Outputs   map[string]string `json:"outputs,omitempty"`
	Artifacts []CASRef          `json:"artifacts,omitempty"`
}

func EncodeOutputManifest(outputs map[string]string, artifacts []CASRef) (string, error) {
	manifest := OutputManifest{Artifacts: append([]CASRef(nil), artifacts...)}
	if len(outputs) > 0 {
		manifest.Outputs = make(map[string]string, len(outputs))
		for key, value := range outputs {
			manifest.Outputs[key] = value
		}
	}
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode output manifest: %w", err)
	}
	return string(encoded), nil
}

func ParseOutputManifest(raw string) (OutputManifest, error) {
	if strings.TrimSpace(raw) == "" {
		return OutputManifest{}, nil
	}
	var manifest OutputManifest
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return OutputManifest{}, fmt.Errorf("decode output manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return OutputManifest{}, err
	}
	return manifest, nil
}

func (m OutputManifest) Validate() error {
	for _, artifact := range m.Artifacts {
		if err := artifact.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (m OutputManifest) ArtifactForOutput(key string) (CASRef, bool) {
	value := strings.TrimSpace(m.Outputs[key])
	if value == "" {
		return CASRef{}, false
	}
	value = strings.TrimPrefix(value, "cas://sha256/")
	value = strings.TrimPrefix(value, "sha256:")
	for _, artifact := range m.Artifacts {
		if artifact.Hex() == value {
			return artifact, true
		}
	}
	return CASRef{}, false
}

func (r StepResult) ResolvedOutputManifest() (string, error) {
	raw := strings.TrimSpace(r.OutputManifest)
	if raw != "" {
		if strings.HasPrefix(raw, "{") {
			manifest, err := ParseOutputManifest(raw)
			if err != nil {
				return "", err
			}
			if err := manifest.Validate(); err != nil {
				return "", err
			}
		}
		return raw, nil
	}
	if len(r.Outputs) == 0 && len(r.Artifacts) == 0 {
		return "", nil
	}
	return EncodeOutputManifest(r.Outputs, r.Artifacts)
}
