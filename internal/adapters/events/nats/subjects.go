package nats

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
)

func validateName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name must not be empty", kind)
	}
	if len(name) > 255 {
		return fmt.Errorf("%s name must be at most 255 characters", kind)
	}
	for _, char := range name {
		alphanumeric := (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
		if !alphanumeric && char != '-' && char != '_' {
			return fmt.Errorf("%s name %q contains an unsupported character", kind, name)
		}
	}
	return nil
}

func consumerName(prefix, consumer string) (string, error) {
	if err := validateName("consumer", consumer); err != nil {
		return "", err
	}
	return prefix + "-" + consumer, nil
}

func normalizeFilterSubjects(subjects []string) ([]string, error) {
	if len(subjects) == 0 {
		return nil, errors.New("at least one subject is required")
	}
	patterns := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		pattern := strings.TrimSpace(subject)
		if !validSubjectPattern(pattern) {
			return nil, fmt.Errorf("invalid subject pattern %q", subject)
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

func validSubjectPattern(pattern string) bool {
	if pattern == "" {
		return false
	}
	tokens := strings.Split(pattern, ".")
	for index, token := range tokens {
		switch token {
		case "":
			return false
		case "*":
		case ">":
			if index != len(tokens)-1 {
				return false
			}
		default:
			if strings.ContainsAny(token, "*> \t\r\n") {
				return false
			}
		}
	}
	return true
}

func matchesSubject(pattern, subject string) bool {
	patternTokens := strings.Split(pattern, ".")
	subjectTokens := strings.Split(subject, ".")
	for index, token := range patternTokens {
		if token == ">" {
			return len(subjectTokens) > index
		}
		if index >= len(subjectTokens) {
			return false
		}
		if token == "*" {
			continue
		}
		if token != subjectTokens[index] {
			return false
		}
	}
	return len(subjectTokens) == len(patternTokens)
}

func matchesAny(subject string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchesSubject(pattern, subject) {
			return true
		}
	}
	return false
}

func marshalEnvelope(envelope domain.EventEnvelope) ([]byte, error) {
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode event envelope: %w", err)
	}
	return data, nil
}

func unmarshalEnvelope(data []byte) (domain.EventEnvelope, error) {
	var envelope domain.EventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return envelope, fmt.Errorf("decode event envelope: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return envelope, fmt.Errorf("validate event envelope: %w", err)
	}
	if len(envelope.Payload) == 0 || !json.Valid(envelope.Payload) {
		return envelope, errors.New("event payload must be valid JSON")
	}
	return envelope, nil
}
