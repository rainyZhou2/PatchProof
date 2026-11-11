package evidence

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func mustCanonicalize(t *testing.T, value any) []byte {
	t.Helper()
	data, err := Canonicalize(value)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return data
}

func TestCanonicalizeSortsKeysAtAllDepths(t *testing.T) {
	value := map[string]any{
		"b": map[string]any{"z": 1, "a": 2},
		"a": map[string]any{"y": true, "x": nil},
	}
	want := `{"a":{"x":null,"y":true},"b":{"a":2,"z":1}}`
	if got := string(mustCanonicalize(t, value)); got != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalizePreservesUnicode(t *testing.T) {
	value := map[string]any{"text": "héllo 世界 🚀"}
	want := `{"text":"héllo 世界 🚀"}`
	if got := string(mustCanonicalize(t, value)); got != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalizeDoesNotEscapeHTML(t *testing.T) {
	value := map[string]any{"html": `<script>alert("x & y")</script>`}
	want := `{"html":"<script>alert(\"x & y\")</script>"}`
	if got := string(mustCanonicalize(t, value)); got != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalizeEscapesControlCharacters(t *testing.T) {
	value := map[string]any{"s": "a\nb\tc\r\bd\fe\u0001f\\g\"h"}
	want := `{"s":"a\nb\tc\r\bd\fe\u0001f\\g\"h"}`
	if got := string(mustCanonicalize(t, value)); got != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalizeNumbers(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"integer stays exact", int64(9007199254740993), "9007199254740993"},
		{"large integer max", int64(math.MaxInt64), "9223372036854775807"},
		{"float integral", 1.0, "1"},
		{"float fraction", 0.5, "0.5"},
		{"small float exponent", 1e-7, "1e-7"},
		{"json number exponent normalized", json.Number("1e3"), "1000"},
		{"json number uppercase exponent", json.Number("1.5E+2"), "150"},
		{"json number negative exponent", json.Number("2.5e-3"), "0.0025"},
		{"zero float", 0.0, "0"},
		{"negative float", -1.25, "-1.25"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(mustCanonicalize(t, tc.value)); got != tc.want {
				t.Fatalf("canonical = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCanonicalizeRejectsNegativeZero(t *testing.T) {
	for _, value := range []any{
		json.Number("-0"),
		math.Copysign(0, -1),
		map[string]any{"v": json.Number("-0")},
	} {
		if _, err := Canonicalize(value); err == nil {
			t.Fatalf("negative zero %v accepted", value)
		}
	}
}

func TestCanonicalizeRejectsInvalidNumberForms(t *testing.T) {
	for _, raw := range []string{"+1", "01", "-01"} {
		if _, err := Canonicalize(json.Number(raw)); err == nil {
			t.Errorf("invalid number %q accepted", raw)
		}
	}
}

func TestCanonicalizeArraysPreserveOrder(t *testing.T) {
	ascending := mustCanonicalize(t, []any{1, 2, 3})
	descending := mustCanonicalize(t, []any{3, 2, 1})
	if bytes.Equal(ascending, descending) {
		t.Fatal("arrays with different order must not canonicalize identically")
	}
	if string(ascending) != "[1,2,3]" {
		t.Fatalf("ascending = %s", ascending)
	}
	nested := mustCanonicalize(t, []any{
		map[string]any{"b": 2, "a": 1},
		map[string]any{"d": 4, "c": 3},
	})
	if string(nested) != `[{"a":1,"b":2},{"c":3,"d":4}]` {
		t.Fatalf("nested array canonical = %s", nested)
	}
}

func TestCanonicalizeStructAndMapEquivalence(t *testing.T) {
	type sample struct {
		B int    `json:"b"`
		A string `json:"a"`
	}
	structBytes := mustCanonicalize(t, sample{B: 2, A: "x"})
	mapBytes := mustCanonicalize(t, map[string]any{"a": "x", "b": 2})
	if !bytes.Equal(structBytes, mapBytes) {
		t.Fatalf("struct %s and map %s differ", structBytes, mapBytes)
	}
}

func TestCanonicalizeNestedStructRoundTrip(t *testing.T) {
	type inner struct {
		Z int `json:"z"`
	}
	type outer struct {
		List []inner `json:"list"`
		B    string  `json:"b"`
		A    bool    `json:"a"`
	}
	got := mustCanonicalize(t, outer{List: []inner{{Z: 1}, {Z: 2}}, B: "x", A: true})
	want := `{"a":true,"b":"x","list":[{"z":1},{"z":2}]}`
	if string(got) != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalizeRejectsUnsupportedTypes(t *testing.T) {
	tests := []any{
		func() {},
		make(chan int),
		complex(1, 2),
		struct{ C complex128 }{C: complex(1, 2)},
	}
	for _, value := range tests {
		if _, err := Canonicalize(value); err == nil {
			t.Errorf("unsupported type %T accepted", value)
		}
	}
}

func TestCanonicalDigest(t *testing.T) {
	first, err := CanonicalDigest(map[string]any{"a": 1, "b": 2})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	second, err := CanonicalDigest(map[string]any{"b": 2, "a": 1})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if first != second {
		t.Fatalf("digests differ for reordered maps: %s vs %s", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("digest shape = %q", first)
	}
	third, err := CanonicalDigest(map[string]any{"a": 1, "b": 3})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if first == third {
		t.Fatal("different values produced the same digest")
	}
}

func TestDigestBytes(t *testing.T) {
	first := DigestBytes([]byte("hello"))
	if first != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("digest = %q", first)
	}
	if first == DigestBytes([]byte("hello!")) {
		t.Fatal("different bytes produced the same digest")
	}
}

func TestReadAllCanonical(t *testing.T) {
	input := strings.NewReader(`{"b":1,"a":{"d":2,"c":3}}`)
	got, err := ReadAllCanonical(input)
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	want := `{"a":{"c":3,"d":2},"b":1}`
	if string(got) != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
	if _, err := ReadAllCanonical(strings.NewReader("not json")); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestCanonicalizeNil(t *testing.T) {
	if got := string(mustCanonicalize(t, nil)); got != "null" {
		t.Fatalf("nil canonical = %s, want null", got)
	}
}
