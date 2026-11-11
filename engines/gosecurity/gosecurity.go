// Package gosecurity implements the bundled Go security-scan engine. The
// checks are intentionally small, explicit and deterministic so a finding is
// reproducible from the snapshot without an external scanner binary.
package gosecurity

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

const (
	EngineName    = "go-security"
	EngineVersion = "1.0.0"
	language      = "go"
)

var privateKeyPattern = regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`)

// Engine implements the bundled Go security scanner.
type Engine struct{}

func New() *Engine { return &Engine{} }

func (e *Engine) Name() string             { return EngineName }
func (e *Engine) Version() string          { return EngineVersion }
func (e *Engine) Kinds() []domain.StepKind { return []domain.StepKind{domain.StepKindSecurity} }
func (e *Engine) Languages() []string      { return []string{language} }

type finding struct {
	Rule    string
	Path    string
	Line    int
	Message string
}

// Execute scans the staged head snapshot for high-confidence insecure Go
// patterns. It deliberately does not attempt data-flow taint analysis; the
// evidence records the rule set and its heuristic limits.
func (e *Engine) Execute(ctx context.Context, request engine.Request) (engine.Result, error) {
	if err := ctx.Err(); err != nil {
		return engine.Result{}, err
	}
	head, ok := request.Staged["head"]
	if !ok || strings.TrimSpace(head.Path) == "" {
		return engine.Result{
			Outcome: domain.OutcomeFailed, ErrorClass: domain.ErrValidationFailed,
			Message: "security scan requires a staged head snapshot", Retryable: false,
		}, nil
	}
	started := time.Now()
	findings, parseErrors, files, err := scanTree(ctx, head.Path)
	if err != nil {
		return engine.Result{
			Outcome: domain.OutcomeFailed, ErrorClass: domain.ErrExecutorError,
			Message: "security scan failed: " + err.Error(), Retryable: true,
		}, nil
	}
	claimID := domain.ClaimID(stepopts.Get(request.Spec.Options, stepopts.ClaimID))
	if claimID == "" {
		claimID = "security-scan"
	}

	result := domain.ClaimTested
	message := fmt.Sprintf("security scan completed with no findings across %d Go files", files)
	if len(findings) > 0 {
		result = domain.ClaimViolated
		message = fmt.Sprintf("security scan found %d high-confidence issue(s)", len(findings))
	} else if len(parseErrors) > 0 || files == 0 {
		result = domain.ClaimUnknown
		message = "security scan could not analyze the complete Go source tree"
	}

	details := map[string]string{
		"files_scanned": strconv.Itoa(files),
		"findings":      strconv.Itoa(len(findings)),
		"parse_errors":  strconv.Itoa(len(parseErrors)),
		"rules":         "INSECURE_TLS_VERIFY, SHELL_COMMAND, WORLD_WRITABLE_FILE, PLAINTEXT_HTTP, PRIVATE_KEY_MATERIAL",
	}
	if len(findings) > 0 {
		limit := len(findings)
		if limit > 20 {
			limit = 20
		}
		lines := make([]string, 0, limit)
		for _, item := range findings[:limit] {
			lines = append(lines, fmt.Sprintf("%s:%d %s %s", item.Path, item.Line, item.Rule, item.Message))
		}
		details["finding_list"] = strings.Join(lines, "; ")
	}
	limits := []string{
		"this bundled scanner is a deterministic heuristic; it is not a replacement for a full SAST or dependency CVE scanner",
	}
	if len(parseErrors) > 0 {
		limits = append(limits, fmt.Sprintf("%d Go file(s) could not be parsed", len(parseErrors)))
	}
	now := time.Now().UTC()
	if request.Clock != nil {
		now = request.Clock.Now().UTC()
	}
	sandbox := string(request.Spec.Requirements.Isolation)
	if sandbox == "" {
		sandbox = "unspecified"
	}
	tenantID := request.TenantID
	if tenantID == "" {
		tenantID = request.Spec.TenantID
	}
	evidence := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      tenantID,
		RunID:         request.Spec.RunID,
		StepID:        request.Spec.StepID,
		ClaimID:       claimID,
		Kind:          domain.EvidenceStatic,
		Result:        result,
		Scope: domain.EvidenceScope{
			InputDomain: "changed packages and their direct dependencies",
			ElapsedMS:   time.Since(started).Milliseconds(),
			Details:     details,
		},
		Assumptions: []string{
			"the staged head snapshot is the complete source tree",
			"generated and vendored files are out of scope",
		},
		Engine: domain.EngineInfo{Name: EngineName, Version: EngineVersion},
		Environment: domain.EnvironmentInfo{
			Arch: runtime.GOARCH, OS: runtime.GOOS, SandboxProfile: sandbox,
		},
		Limits:    limits,
		CreatedAt: now,
	}
	return engine.Result{
		Outcome:  domain.OutcomeSucceeded,
		Message:  message,
		Evidence: []domain.Evidence{evidence},
		Metrics:  domain.StepMetrics{ExecutionMS: time.Since(started).Milliseconds()},
		Outputs:  map[string]string{"findings": strconv.Itoa(len(findings))},
	}, nil
}

func scanTree(ctx context.Context, root string) ([]finding, []string, int, error) {
	var findings []finding
	var parseErrors []string
	files := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if privateKeyPattern.Match(source) {
			findings = append(findings, finding{Rule: "PRIVATE_KEY_MATERIAL", Path: relative, Message: "private key material is committed in source"})
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, source, 0)
		if err != nil {
			parseErrors = append(parseErrors, relative+": "+err.Error())
			return nil
		}
		files++
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CompositeLit:
				if isTLSConfig(typed) {
					for _, element := range typed.Elts {
						keyValue, ok := element.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := keyValue.Key.(*ast.Ident)
						value, valueOK := keyValue.Value.(*ast.Ident)
						if ok && valueOK && key.Name == "InsecureSkipVerify" && value.Name == "true" {
							findings = append(findings, finding{Rule: "INSECURE_TLS_VERIFY", Path: relative, Line: fset.Position(keyValue.Pos()).Line, Message: "TLS certificate verification is disabled"})
						}
					}
				}
			case *ast.CallExpr:
				if rule, position, message, ok := inspectCall(typed); ok {
					findings = append(findings, finding{Rule: rule, Path: relative, Line: fset.Position(position).Line, Message: message})
				}
			}
			return true
		})
		return nil
	})
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Rule < findings[j].Rule
	})
	return findings, parseErrors, files, err
}

func isTLSConfig(literal *ast.CompositeLit) bool {
	selector, ok := literal.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "tls" && selector.Sel.Name == "Config"
}

func inspectCall(call *ast.CallExpr) (rule string, position token.Pos, message string, ok bool) {
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector {
		return "", 0, "", false
	}
	pkg, isIdent := selector.X.(*ast.Ident)
	if !isIdent {
		return "", 0, "", false
	}
	switch {
	case pkg.Name == "exec" && selector.Sel.Name == "Command":
		if len(call.Args) >= 2 {
			shell, shellOK := call.Args[0].(*ast.BasicLit)
			flag, flagOK := call.Args[1].(*ast.BasicLit)
			if shellOK && flagOK && (shell.Value == `"sh"` || shell.Value == `"bash"` || shell.Value == `"/bin/sh"` || shell.Value == `"/bin/bash"`) && flag.Value == `"-c"` {
				return "SHELL_COMMAND", call.Pos(), "shell command is constructed with -c", true
			}
		}
	case pkg.Name == "os" && selector.Sel.Name == "OpenFile":
		if len(call.Args) == 3 {
			if literal, literalOK := call.Args[2].(*ast.BasicLit); literalOK && (literal.Value == "0777" || literal.Value == "0666") {
				return "WORLD_WRITABLE_FILE", call.Pos(), "file is created with world-writable permissions", true
			}
		}
	case pkg.Name == "http" && selector.Sel.Name == "ListenAndServe":
		return "PLAINTEXT_HTTP", call.Pos(), "plaintext HTTP server has no TLS configuration", true
	}
	return "", 0, "", false
}
