package policy

import (
	"reflect"
	"testing"
)

func TestGlobDoubleStarAcrossDirectories(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "pkg/a/b.go", true},
		{"**/*.go", "pkg/a/b.txt", false},
		{"pkg/**/testdata/**", "pkg/testdata/x.json", true},
		{"pkg/**/testdata/**", "pkg/a/b/testdata/x.json", true},
		{"pkg/**/testdata/**", "pkg/a/b/fixtures/x.json", false},
		{"docs/**", "docs/index.md", true},
		{"docs/**", "docs/a/b/c.md", true},
		{"docs/**", "docsx/index.md", false},
		{"**", "anything/at/all.txt", true},
	}
	for _, tc := range tests {
		matcher := mustMatcher(t, []string{tc.pattern})
		if got := matcher.Match(tc.path); got != tc.want {
			t.Errorf("pattern %q match %q = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestGlobSingleStarIsSingleSegment(t *testing.T) {
	matcher := mustMatcher(t, []string{"src/*.go"})
	if !matcher.Match("src/main.go") {
		t.Error("single-segment match failed")
	}
	if matcher.Match("src/nested/main.go") {
		t.Error("* must not cross directories")
	}
	if matcher.Match("src/main.go.bak") {
		t.Error("* must not match across the extension")
	}

	anchored := mustMatcher(t, []string{"*.go"})
	if !anchored.Match("main.go") {
		t.Error("root match failed")
	}
	if anchored.Match("dir/main.go") {
		t.Error("pattern must be anchored at the root")
	}
	if anchored.Match("main.go/extra") {
		t.Error("pattern must be anchored at the end")
	}
}

func TestGlobQuestionMarkAndCharacterClasses(t *testing.T) {
	question := mustMatcher(t, []string{"file?.txt"})
	for path, want := range map[string]bool{
		"file1.txt":     true,
		"fileA.txt":     true,
		"file.txt":      false,
		"file10.txt":    false,
		"dir/file1.txt": false,
	} {
		if got := question.Match(path); got != want {
			t.Errorf("? match %q = %v, want %v", path, got, want)
		}
	}

	class := mustMatcher(t, []string{"[a-c].go"})
	for path, want := range map[string]bool{
		"a.go": true,
		"b.go": true,
		"c.go": true,
		"d.go": false,
	} {
		if got := class.Match(path); got != want {
			t.Errorf("class match %q = %v, want %v", path, got, want)
		}
	}
}

func TestGlobRejectsUnsafePatterns(t *testing.T) {
	invalid := []string{
		"",
		"   ",
		"/etc/passwd",
		"../secret",
		"a/../b",
		"..",
		"[unclosed",
	}
	for _, pattern := range invalid {
		if _, err := NewPathMatcher([]string{pattern}); err == nil {
			t.Errorf("pattern %q accepted", pattern)
		}
	}
	if _, err := NewPathMatcher([]string{"valid/**", "/absolute"}); err == nil {
		t.Error("matcher with one unsafe pattern accepted")
	}
}

func TestPathMatcherNormalizesPaths(t *testing.T) {
	matcher := mustMatcher(t, []string{"pkg/*.go"})
	if !matcher.Match(`pkg\main.go`) {
		t.Error("backslash path not normalized")
	}
	if !matcher.Match("./pkg/main.go") {
		t.Error("./ prefix not stripped")
	}
}

func TestPathMatcherMatchesPreservesInputOrder(t *testing.T) {
	matcher := mustMatcher(t, []string{"**/*.go"})
	paths := []string{"z.go", "readme.md", "a/b.go"}
	got := matcher.Matches(paths)
	want := []string{"z.go", "a/b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matches = %v, want %v", got, want)
	}
	if matched := matcher.Matches(nil); matched != nil {
		t.Fatalf("empty matches = %v, want nil", matched)
	}
}

func TestNilPathMatcher(t *testing.T) {
	var matcher *PathMatcher
	if matcher.Match("anything") {
		t.Error("nil matcher matched a path")
	}
	if matcher.Matches([]string{"anything"}) != nil {
		t.Error("nil matcher returned matches")
	}
}

func TestNewPathMatcherEmpty(t *testing.T) {
	matcher, err := NewPathMatcher(nil)
	if err != nil {
		t.Fatalf("empty matcher: %v", err)
	}
	if matcher.Match("anything") {
		t.Error("empty matcher matched a path")
	}
}

func mustMatcher(t *testing.T, patterns []string) *PathMatcher {
	t.Helper()
	matcher, err := NewPathMatcher(patterns)
	if err != nil {
		t.Fatalf("new path matcher: %v", err)
	}
	return matcher
}
