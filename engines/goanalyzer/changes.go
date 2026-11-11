package goanalyzer

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// declInfo captures the semantic fingerprint of one top-level declaration.
type declInfo struct {
	symbol      Symbol
	signature   string
	body        string
	returns     string
	conditions  string
	errors      string
	concurrency string
}

// fingerprintLimit bounds each per-declaration fingerprint.
const fingerprintLimit = 64 << 10

// fingerprintBuffer appends tokens until a size bound is reached.
type fingerprintBuffer struct {
	builder   strings.Builder
	truncated bool
}

// add appends a token while respecting fingerprintLimit.
func (b *fingerprintBuffer) add(token string) {
	if b.truncated {
		return
	}
	if b.builder.Len()+len(token)+1 > fingerprintLimit {
		b.truncated = true
		return
	}
	if b.builder.Len() > 0 {
		b.builder.WriteByte('|')
	}
	b.builder.WriteString(token)
}

// String returns the accumulated fingerprint.
func (b *fingerprintBuffer) String() string {
	if b.truncated {
		return b.builder.String() + "|TRUNCATED"
	}
	return b.builder.String()
}

// nodeFingerprint renders a structural, position-independent AST fingerprint.
func nodeFingerprint(node ast.Node) string {
	buffer := &fingerprintBuffer{}
	ast.Inspect(node, func(current ast.Node) bool {
		if current == nil {
			return true
		}
		switch typed := current.(type) {
		case *ast.Ident:
			buffer.add("I:" + typed.Name)
		case *ast.BasicLit:
			buffer.add("L:" + typed.Kind.String() + ":" + typed.Value)
		case *ast.BinaryExpr:
			buffer.add("B:" + typed.Op.String())
		case *ast.UnaryExpr:
			buffer.add("U:" + typed.Op.String())
		case *ast.AssignStmt:
			buffer.add("A:" + typed.Tok.String())
		case *ast.IncDecStmt:
			buffer.add("D:" + typed.Tok.String())
		case *ast.BranchStmt:
			buffer.add("BR:" + typed.Tok.String())
		case *ast.SelectorExpr:
			buffer.add("S:" + typed.Sel.Name)
		case *ast.CallExpr:
			buffer.add("CALL")
		case *ast.StarExpr:
			buffer.add("PTR")
		case *ast.FuncLit:
			buffer.add("FUNCLIT")
		default:
			buffer.add(fmt.Sprintf("%T", current))
		}
		return true
	})
	return buffer.String()
}

// returnFingerprint records the return expressions of a body.
func returnFingerprint(fset *token.FileSet, body *ast.BlockStmt) string {
	buffer := &fingerprintBuffer{}
	if body == nil {
		return ""
	}
	ast.Inspect(body, func(node ast.Node) bool {
		statement, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if len(statement.Results) == 0 {
			buffer.add("RETURN")
			return true
		}
		for _, result := range statement.Results {
			buffer.add("RET:" + printNode(fset, result))
		}
		return true
	})
	return buffer.String()
}

// conditionFingerprint records control-flow conditions and case expressions.
func conditionFingerprint(fset *token.FileSet, body *ast.BlockStmt) string {
	buffer := &fingerprintBuffer{}
	if body == nil {
		return ""
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.IfStmt:
			if typed.Init != nil {
				buffer.add("IFINIT:" + printNode(fset, typed.Init))
			}
			buffer.add("IF:" + printNode(fset, typed.Cond))
		case *ast.ForStmt:
			if typed.Init != nil {
				buffer.add("FORINIT:" + printNode(fset, typed.Init))
			}
			if typed.Cond != nil {
				buffer.add("FOR:" + printNode(fset, typed.Cond))
			}
			if typed.Post != nil {
				buffer.add("FORPOST:" + printNode(fset, typed.Post))
			}
		case *ast.RangeStmt:
			buffer.add("RANGE:" + printNode(fset, typed.X))
		case *ast.SwitchStmt:
			if typed.Tag != nil {
				buffer.add("SWITCH:" + printNode(fset, typed.Tag))
			}
		case *ast.TypeSwitchStmt:
			if typed.Assign != nil {
				buffer.add("TSWITCH:" + printNode(fset, typed.Assign))
			}
		case *ast.CaseClause:
			for _, expression := range typed.List {
				buffer.add("CASE:" + printNode(fset, expression))
			}
		}
		return true
	})
	return buffer.String()
}

// errorFingerprint records error-handling shapes: error checks, error
// construction, and error returns.
func errorFingerprint(fset *token.FileSet, body *ast.BlockStmt) string {
	buffer := &fingerprintBuffer{}
	if body == nil {
		return ""
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.IfStmt:
			condition := printNode(fset, typed.Cond)
			if mentionsError(condition) {
				buffer.add("ERRCHECK:" + condition)
			}
		case *ast.CallExpr:
			name := callName(typed.Fun)
			if errorHandlingNames[name] {
				buffer.add("ERRCALL:" + name)
			}
		case *ast.ReturnStmt:
			for _, result := range typed.Results {
				text := printNode(fset, result)
				if mentionsError(text) {
					buffer.add("ERRRET:" + text)
				}
			}
		}
		return true
	})
	return buffer.String()
}

// concurrencyFingerprint records goroutines, channels, selects and
// synchronization primitives.
func concurrencyFingerprint(fset *token.FileSet, body *ast.BlockStmt) string {
	buffer := &fingerprintBuffer{}
	if body == nil {
		return ""
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.GoStmt:
			buffer.add("GO:" + printNode(fset, typed.Call.Fun))
		case *ast.SelectStmt:
			buffer.add("SELECT")
		case *ast.ChanType:
			buffer.add("CHAN:" + strconv.Itoa(int(typed.Dir)))
		case *ast.SendStmt:
			buffer.add("SEND:" + printNode(fset, typed.Chan))
		case *ast.SelectorExpr:
			identifier, ok := typed.X.(*ast.Ident)
			if ok && synchronizationPackages[identifier.Name] {
				buffer.add("SYNC:" + identifier.Name + "." + typed.Sel.Name)
			}
		}
		return true
	})
	return buffer.String()
}

// mentionsError reports whether an expression text refers to an error value.
func mentionsError(text string) bool {
	return strings.Contains(text, "err") || strings.Contains(text, "Err") ||
		strings.Contains(text, "error") || strings.Contains(text, "Error")
}

// callName returns a readable name for a called expression.
func callName(expr ast.Expr) string {
	switch typed := unwrapCallTarget(expr).(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

// errorHandlingNames are call names that construct or inspect errors.
var errorHandlingNames = map[string]bool{
	"New": true, "Errorf": true, "Wrap": true, "Wrapf": true, "Join": true,
	"Is": true, "As": true, "Unwrap": true, "Fatal": true, "Fatalf": true,
	"Panic": true, "Panicf": true, "recover": true, "panic": true,
}

// synchronizationPackages are package identifiers treated as concurrency
// primitives when they appear as selector receivers.
var synchronizationPackages = map[string]bool{
	"sync": true, "atomic": true, "context": true, "errgroup": true,
	"semaphore": true, "singleflight": true,
}

// parseDeclInfos parses one file and produces semantic declarations.
func parseDeclInfos(root, relativeFile string, content []byte, modulePath string) ([]declInfo, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relativeFile, content, parser.SkipObjectResolution)
	if file == nil {
		return nil, fmt.Errorf("parse %s: %w", relativeFile, err)
	}
	pkgPath := packagePathFor(root, relativeFile, modulePath)
	var declarations []declInfo
	for _, declaration := range file.Decls {
		switch node := declaration.(type) {
		case *ast.FuncDecl:
			if node.Name == nil || node.Name.Name == "_" {
				continue
			}
			kind := SymbolKindFunc
			id := pkgPath + "#" + node.Name.Name
			if node.Recv != nil && len(node.Recv.List) > 0 {
				if receiver := astReceiverName(node.Recv.List[0].Type); receiver != "" {
					kind = SymbolKindMethod
					id = pkgPath + "#" + receiver + "." + node.Name.Name
				}
			}
			declarations = append(declarations, declInfo{
				symbol: Symbol{
					ID:          id,
					PackagePath: pkgPath,
					Name:        node.Name.Name,
					Kind:        kind,
					Signature:   astFuncSignature(fset, node),
					File:        relativeFile,
					Line:        fset.Position(node.Pos()).Line,
					Exported:    node.Name.IsExported(),
				},
				signature:   astFuncSignature(fset, node),
				body:        nodeFingerprint(node.Body),
				returns:     returnFingerprint(fset, node.Body),
				conditions:  conditionFingerprint(fset, node.Body),
				errors:      errorFingerprint(fset, node.Body),
				concurrency: concurrencyFingerprint(fset, node.Body),
			})
		case *ast.GenDecl:
			for _, specification := range node.Specs {
				switch spec := specification.(type) {
				case *ast.TypeSpec:
					if spec.Name == nil || spec.Name.Name == "_" {
						continue
					}
					signature := astTypeSignature(fset, spec)
					declarations = append(declarations, declInfo{
						symbol: Symbol{
							ID:          pkgPath + "#" + spec.Name.Name,
							PackagePath: pkgPath,
							Name:        spec.Name.Name,
							Kind:        SymbolKindType,
							Signature:   signature,
							File:        relativeFile,
							Line:        fset.Position(spec.Pos()).Line,
							Exported:    spec.Name.IsExported(),
						},
						signature: normalizeSignature("type " + spec.Name.Name + " " + printNode(fset, spec.Type)),
					})
				case *ast.ValueSpec:
					names := make([]string, 0, len(spec.Names))
					for _, name := range spec.Names {
						if name.Name != "_" {
							names = append(names, name.Name)
						}
					}
					if len(names) == 0 {
						continue
					}
					kind := SymbolKindVar
					if node.Tok == token.CONST {
						kind = SymbolKindConst
					}
					signature := astValueSignature(node.Tok, names, spec.Type)
					for _, name := range spec.Names {
						if name.Name == "_" {
							continue
						}
						declarations = append(declarations, declInfo{
							symbol: Symbol{
								ID:          pkgPath + "#" + name.Name,
								PackagePath: pkgPath,
								Name:        name.Name,
								Kind:        kind,
								Signature:   signature,
								File:        relativeFile,
								Line:        fset.Position(name.Pos()).Line,
								Exported:    name.IsExported(),
							},
							signature: signature,
							body:      nodeFingerprint(spec),
						})
					}
				}
			}
		}
	}
	return declarations, nil
}

// declKey identifies a declaration independently of its package path, which
// allows matching symbols across file moves between packages.
func declKey(declaration declInfo) string {
	id := declaration.symbol.ID
	if idx := strings.Index(id, "#"); idx >= 0 {
		id = id[idx+1:]
	}
	return declaration.symbol.Kind + "|" + id
}

// classifyDeclChange derives semantic kinds for one modified declaration.
func classifyDeclChange(base, head declInfo) []string {
	kinds := make([]string, 0, 4)
	if base.signature != head.signature {
		kinds = append(kinds, SemanticSignatureChanged)
	}
	if head.symbol.Kind == SymbolKindFunc || head.symbol.Kind == SymbolKindMethod {
		if base.returns != head.returns {
			kinds = append(kinds, SemanticReturnChanged)
		}
		if base.conditions != head.conditions {
			kinds = append(kinds, SemanticConditionChanged)
		}
		if base.errors != head.errors {
			kinds = append(kinds, SemanticErrorChanged)
		}
		if base.concurrency != head.concurrency {
			kinds = append(kinds, SemanticConcurrency)
		}
		if base.body != head.body && len(kinds) == 0 {
			kinds = append(kinds, SemanticBodyChanged)
		}
	} else if base.signature != head.signature || base.body != head.body {
		kinds = append(kinds, SemanticValueChanged)
	}
	return kinds
}

// compareDeclarations returns changed symbols and aggregated semantic kinds.
func compareDeclarations(base, head []declInfo) ([]string, []string) {
	headByID := make(map[string]declInfo, len(head))
	headByKey := make(map[string]declInfo, len(head))
	for _, declaration := range head {
		headByID[declaration.symbol.ID] = declaration
		headByKey[declKey(declaration)] = declaration
	}
	matched := make(map[string]struct{}, len(head))
	symbolSet := make(map[string]struct{})
	kindSet := make(map[string]struct{})
	for _, baseDeclaration := range base {
		headDeclaration, ok := headByID[baseDeclaration.symbol.ID]
		if !ok {
			headDeclaration, ok = headByKey[declKey(baseDeclaration)]
		}
		if !ok {
			symbolSet[baseDeclaration.symbol.ID] = struct{}{}
			kindSet[SemanticSymbolDeleted] = struct{}{}
			if baseDeclaration.symbol.Exported {
				kindSet[SemanticSignatureChanged] = struct{}{}
			}
			continue
		}
		matched[headDeclaration.symbol.ID] = struct{}{}
		if baseDeclaration.signature == headDeclaration.signature &&
			baseDeclaration.body == headDeclaration.body &&
			baseDeclaration.returns == headDeclaration.returns &&
			baseDeclaration.conditions == headDeclaration.conditions &&
			baseDeclaration.errors == headDeclaration.errors &&
			baseDeclaration.concurrency == headDeclaration.concurrency {
			continue
		}
		symbolSet[headDeclaration.symbol.ID] = struct{}{}
		for _, kind := range classifyDeclChange(baseDeclaration, headDeclaration) {
			kindSet[kind] = struct{}{}
		}
	}
	for _, headDeclaration := range head {
		if _, ok := matched[headDeclaration.symbol.ID]; ok {
			continue
		}
		symbolSet[headDeclaration.symbol.ID] = struct{}{}
		kindSet[SemanticSymbolAdded] = struct{}{}
		if headDeclaration.symbol.Exported {
			kindSet[SemanticSignatureChanged] = struct{}{}
		}
	}
	return sortedKeys(symbolSet), sortedKeys(kindSet)
}

// sortedKeys returns the sorted keys of a string set.
func sortedKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// dependencyFile reports whether a changed path contributes to dependency
// classification.
func dependencyFile(path string) bool {
	switch filepath.Base(path) {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	default:
		return false
	}
}

// buildChanges compares snapshots and produces semantic changes plus the set of
// changed symbols.
func buildChanges(ctx context.Context, baseRoot, headRoot, baseModule, headModule string, diffs []FileDiff, baseIndex, headIndex symbolIndex, limits *limitSet) ([]Change, []string) {
	changes := make([]Change, 0, len(diffs))
	changedSymbols := make(map[string]struct{})
	testFilesSkipped := false
	for _, diff := range diffs {
		if err := ctx.Err(); err != nil {
			limits.add("change detection stopped early because the step deadline expired")
			break
		}
		change := Change{
			Path:          diff.Path,
			Kind:          diff.Change,
			BaseDigest:    diff.BaseDigest,
			HeadDigest:    diff.HeadDigest,
			Symbols:       []string{},
			SemanticKinds: []string{},
		}
		if dependencyFile(diff.Path) {
			change.SemanticKinds = []string{SemanticDependency}
			changes = append(changes, change)
			continue
		}
		if diff.Binary || !isGoSource(diff.Path) || isVendoredPath(diff.Path) {
			if isTestSource(diff.Path) && !testFilesSkipped {
				limits.add("test-only file changes are not represented as IR symbols")
				testFilesSkipped = true
			}
			changes = append(changes, change)
			continue
		}

		baseDecls, baseErr := parseChangeSide(baseRoot, diff, true, baseModule)
		headDecls, headErr := parseChangeSide(headRoot, diff, false, headModule)
		if baseErr != nil && diff.Change != "ADDED" {
			limits.add(fmt.Sprintf("semantic classification used file symbols for %s: %v", diff.Path, baseErr))
		}
		if headErr != nil && diff.Change != "DELETED" {
			limits.add(fmt.Sprintf("semantic classification used file symbols for %s: %v", diff.Path, headErr))
		}

		switch diff.Change {
		case "ADDED":
			if headErr != nil {
				change.Symbols = headIndex.symbolsForFile(diff.Path)
				break
			}
			change.Symbols, change.SemanticKinds = addedDeclarationSummary(headDecls)
		case "DELETED":
			if baseErr != nil {
				change.Symbols = baseIndex.symbolsForFile(diff.Path)
				break
			}
			change.Symbols, change.SemanticKinds = deletedDeclarationSummary(baseDecls)
		default:
			if baseErr != nil || headErr != nil {
				change.Symbols = unionStrings(baseIndex.symbolsForFile(diff.OldPath), headIndex.symbolsForFile(diff.Path))
				break
			}
			change.Symbols, change.SemanticKinds = compareDeclarations(baseDecls, headDecls)
		}
		change.Symbols = dedupeStrings(change.Symbols)
		change.SemanticKinds = dedupeStrings(change.SemanticKinds)
		for _, symbol := range change.Symbols {
			changedSymbols[symbol] = struct{}{}
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	if len(changes) > maxChanges {
		limits.add(fmt.Sprintf("change list capped at %d entries", maxChanges))
		changes = changes[:maxChanges]
	}
	return changes, sortedKeys(changedSymbols)
}

// parseChangeSide parses the base or head copy of a changed file.
func parseChangeSide(root string, diff FileDiff, base bool, modulePath string) ([]declInfo, error) {
	relative := diff.Path
	if base {
		if diff.Change == "DELETED" {
			relative = diff.Path
		}
		if diff.OldPath != "" {
			relative = diff.OldPath
		}
		if relative == "" {
			return nil, fmt.Errorf("no base path for %s", diff.Path)
		}
	}
	if root == "" {
		return nil, fmt.Errorf("no snapshot root for %s", relative)
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	content, bounded, err := readFileBounded(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", relative, err)
	}
	if bounded {
		return nil, fmt.Errorf("%s exceeds the %d byte analysis cap", relative, maxFileBytes)
	}
	return parseDeclInfos(root, relative, content, modulePath)
}

// addedDeclarationSummary describes a newly added file.
func addedDeclarationSummary(declarations []declInfo) ([]string, []string) {
	symbols := make([]string, 0, len(declarations))
	kinds := []string{SemanticSymbolAdded}
	for _, declaration := range declarations {
		symbols = append(symbols, declaration.symbol.ID)
		if declaration.symbol.Exported {
			kinds = append(kinds, SemanticSignatureChanged)
		}
	}
	return symbols, kinds
}

// deletedDeclarationSummary describes a removed file.
func deletedDeclarationSummary(declarations []declInfo) ([]string, []string) {
	symbols := make([]string, 0, len(declarations))
	kinds := []string{SemanticSymbolDeleted}
	for _, declaration := range declarations {
		symbols = append(symbols, declaration.symbol.ID)
		if declaration.symbol.Exported {
			kinds = append(kinds, SemanticSignatureChanged)
		}
	}
	return symbols, kinds
}

// unionStrings merges two string slices without duplicates.
func unionStrings(left, right []string) []string {
	merged := make([]string, 0, len(left)+len(right))
	merged = append(merged, left...)
	merged = append(merged, right...)
	return dedupeStrings(merged)
}
