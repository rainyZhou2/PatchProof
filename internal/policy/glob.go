package policy

import (
	"fmt"
	"regexp"
	"strings"
)

type PathMatcher struct {
	patterns []compiledPattern
}

type compiledPattern struct {
	source string
	re     *regexp.Regexp
}

func NewPathMatcher(patterns []string) (*PathMatcher, error) {
	matcher := &PathMatcher{}
	for _, pattern := range patterns {
		compiled, err := compilePattern(pattern)
		if err != nil {
			return nil, err
		}
		matcher.patterns = append(matcher.patterns, compiled)
	}
	return matcher, nil
}

func compilePattern(pattern string) (compiledPattern, error) {
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" {
		return compiledPattern{}, fmt.Errorf("empty path pattern")
	}
	if strings.HasPrefix(trimmed, "/") || strings.Contains(trimmed, "..") {
		return compiledPattern{}, fmt.Errorf("path pattern %q must be relative and must not contain ..", pattern)
	}
	expression := globToRegexp(trimmed)
	re, err := regexp.Compile(expression)
	if err != nil {
		return compiledPattern{}, fmt.Errorf("path pattern %q: %w", pattern, err)
	}
	return compiledPattern{source: trimmed, re: re}, nil
}

func globToRegexp(pattern string) string {
	var builder strings.Builder
	builder.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					builder.WriteString("(?:.*/)?")
				} else {
					builder.WriteString(".*")
				}
			} else {
				builder.WriteString("[^/]*")
			}
		case '?':
			builder.WriteString("[^/]")
		case '[', ']':
			builder.WriteByte(c)
		default:
			builder.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	builder.WriteString("$")
	return builder.String()
}

func (m *PathMatcher) Match(path string) bool {
	if m == nil {
		return false
	}
	cleaned := strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./")
	for _, pattern := range m.patterns {
		if pattern.re.MatchString(cleaned) {
			return true
		}
	}
	return false
}

func (m *PathMatcher) Matches(paths []string) []string {
	if m == nil {
		return nil
	}
	var matched []string
	for _, path := range paths {
		if m.Match(path) {
			matched = append(matched, path)
		}
	}
	return matched
}
