package memory

import (
	"errors"
	"fmt"
	"strings"
)

func normalizeSubjects(subjects []string) ([]string, error) {
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
