package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fixtureModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/harnessfixture\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "calc", "calc.go"), `package calc

import (
	"errors"
	"fmt"
	"os"
)

type Payload struct {
	Value int `+"`json:\"value\"`"+`
}

func Apply(x int, s string) int {
	if x < 0 {
		panic("negative")
	}
	return x + len(s)
}

func Divide(x int) (int, error) {
	if x == 0 {
		return 0, errors.New("division by zero")
	}
	return 10 / x, nil
}

func EchoFloat(x float64) float64 { return x }

func Noisy(x int) int {
	fmt.Println("target noise on stdout")
	fmt.Fprintln(os.Stderr, "target noise on stderr")
	return x
}

func Unsafe() func() { return func() {} }

func PayloadValue(p *Payload) int {
	if p == nil {
		return -1
	}
	return p.Value
}
`)
	return dir
}

func buildFixtureHarness(t *testing.T, snapshot, workspace, function string) (Environment, string, Signature) {
	t.Helper()
	ctx := context.Background()
	module, err := ModulePath(snapshot)
	if err != nil {
		t.Fatalf("module path: %v", err)
	}
	if _, err := PackageDir(snapshot, module, "example.com/harnessfixture/calc"); err != nil {
		t.Fatalf("package dir: %v", err)
	}
	source, err := GenerateSource("example.com/harnessfixture/calc", function)
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	cleanup, err := WriteSource(snapshot, source)
	if err != nil {
		t.Fatalf("write source: %v", err)
	}
	t.Cleanup(cleanup)
	environment, err := NewEnvironment(ctx, workspace, "off")
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	binary := filepath.Join(workspace, "bin", "harness")
	build := environment.Build(ctx, snapshot, binary, "./.patchproof/harness", 2*time.Minute)
	if !build.Successful() {
		t.Fatalf("build harness: %s", build.Diagnostic())
	}
	signature, _, err := environment.Describe(ctx, binary, snapshot, time.Minute)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	return environment, binary, signature
}

func TestHarnessProtocol(t *testing.T) {
	snapshot := fixtureModule(t)
	workspace := t.TempDir()
	environment, binary, signature := buildFixtureHarness(t, snapshot, workspace, "Apply")

	if signature.Arity != 2 {
		t.Fatalf("arity = %d, want 2", signature.Arity)
	}
	if signature.Params[0].Kind != "int" || signature.Params[0].Bits != 64 {
		t.Fatalf("unexpected int descriptor: %+v", signature.Params[0])
	}
	if signature.Params[1].Kind != "string" {
		t.Fatalf("unexpected string descriptor: %+v", signature.Params[1])
	}

	ctx := context.Background()
	run := func(stdin string, batch bool) []Observation {
		t.Helper()
		result := environment.RunHarness(ctx, binary, snapshot, []byte(stdin), batch, 30*time.Second, 1<<20)
		if !result.Successful() {
			t.Fatalf("run harness: %s", result.Diagnostic())
		}
		observations, err := ParseObservations(result.Stdout, -1)
		if err != nil {
			t.Fatalf("parse observations: %v", err)
		}
		return observations
	}

	observations := run(`[2,"ab"]`, false)
	if len(observations) != 1 || observations[0].Status != StatusReturn {
		t.Fatalf("unexpected single observation: %+v", observations)
	}
	if string(observations[0].Value) != "[4]" {
		t.Fatalf("value = %s, want [4]", observations[0].Value)
	}

	observations = run("[1,\"a\"]\n[-1,\"\"]\n[\"bad\",\"\"]\n", true)
	if len(observations) != 3 {
		t.Fatalf("got %d batch observations, want 3", len(observations))
	}
	if observations[0].Status != StatusReturn {
		t.Fatalf("first batch observation: %+v", observations[0])
	}
	if observations[1].Status != StatusPanic || !strings.Contains(observations[1].Panic, "negative") {
		t.Fatalf("second batch observation: %+v", observations[1])
	}
	if observations[2].Status != StatusInvalid {
		t.Fatalf("third batch observation: %+v", observations[2])
	}

	canonical, err := observations[0].Canonical()
	if err != nil {
		t.Fatalf("canonical observation: %v", err)
	}
	if strings.Contains(string(canonical), "elapsed_ms") {
		t.Fatalf("canonical observation must not contain timing: %s", canonical)
	}
}

func TestHarnessSpecialValues(t *testing.T) {
	snapshot := fixtureModule(t)
	workspace := t.TempDir()
	environment, binary, _ := buildFixtureHarness(t, snapshot, workspace, "EchoFloat")
	ctx := context.Background()

	result := environment.RunHarness(ctx, binary, snapshot, []byte(`[{"__float__":"NaN"}]`+"\n"+`[{"__float__":"+Inf"}]`+"\n"+`[1.5]`), true, 30*time.Second, 1<<20)
	if !result.Successful() {
		t.Fatalf("run harness: %s", result.Diagnostic())
	}
	observations, err := ParseObservations(result.Stdout, 3)
	if err != nil {
		t.Fatalf("parse observations: %v", err)
	}
	var values []map[string]string
	if err := json.Unmarshal(observations[0].Value, &values); err != nil {
		t.Fatalf("decode NaN observation: %v", err)
	}
	if values[0]["__unserializable__"] != "float64:NaN" {
		t.Fatalf("unexpected NaN encoding: %v", values)
	}
	if !strings.Contains(string(observations[1].Value), "float64:+Inf") {
		t.Fatalf("unexpected +Inf encoding: %s", observations[1].Value)
	}
	if string(observations[2].Value) != "[1.5]" {
		t.Fatalf("unexpected float encoding: %s", observations[2].Value)
	}
}

func TestHarnessTargetStdoutIsolated(t *testing.T) {
	snapshot := fixtureModule(t)
	workspace := t.TempDir()
	environment, binary, _ := buildFixtureHarness(t, snapshot, workspace, "Noisy")
	ctx := context.Background()

	result := environment.RunHarness(ctx, binary, snapshot, []byte("[2]\n[3]\n"), true, 30*time.Second, 1<<20)
	if !result.Successful() {
		t.Fatalf("run harness: %s", result.Diagnostic())
	}
	observations, err := ParseObservations(result.Stdout, 2)
	if err != nil {
		t.Fatalf("target stdout corrupted the protocol: %v", err)
	}
	if string(observations[0].Value) != "[2]" || string(observations[1].Value) != "[3]" {
		t.Fatalf("unexpected noisy observations: %+v", observations)
	}
	if !strings.Contains(string(result.Stderr), "target noise on stderr") {
		t.Fatalf("target stderr was not preserved: %q", string(result.Stderr))
	}
}

func TestHarnessErrorAndUnserializable(t *testing.T) {
	snapshot := fixtureModule(t)
	workspace := t.TempDir()
	ctx := context.Background()

	environment, binary, _ := buildFixtureHarness(t, snapshot, workspace, "Divide")
	result := environment.RunHarness(ctx, binary, snapshot, []byte("[0]\n[2]\n"), true, 30*time.Second, 1<<20)
	if !result.Successful() {
		t.Fatalf("run divide harness: %s", result.Diagnostic())
	}
	observations, err := ParseObservations(result.Stdout, 2)
	if err != nil {
		t.Fatalf("parse divide observations: %v", err)
	}
	if !strings.Contains(string(observations[0].Value), "__error__") || !strings.Contains(string(observations[0].Value), "division by zero") {
		t.Fatalf("unexpected error encoding: %s", observations[0].Value)
	}
	if string(observations[1].Value) != "[5,null]" {
		t.Fatalf("unexpected divide value: %s", observations[1].Value)
	}

	if _, err := ParseObservations([]byte("not json\n"), -1); err == nil {
		t.Fatal("expected malformed observation to fail")
	}
}
