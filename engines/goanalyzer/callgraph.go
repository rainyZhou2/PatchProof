package goanalyzer

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"

	"golang.org/x/tools/go/packages"
)

// callResolution is the outcome of resolving one call expression.
type callResolution struct {
	callee     string
	candidates []string
	resolution string
	confidence float64
	reason     string
	skip       bool
}

// exactCall returns an exact resolution.
func exactCall(callee string) callResolution {
	if callee == "" {
		return callResolution{resolution: ResolutionUnknown, reason: "call target has no stable symbol identifier"}
	}
	return callResolution{callee: callee, resolution: ResolutionExact, confidence: 1.0}
}

// approxCalls returns an approximate resolution to matching candidates.
func approxCalls(candidates []string, confidence float64, reason string) callResolution {
	if len(candidates) == 0 {
		return callResolution{resolution: ResolutionUnknown, reason: reason}
	}
	if len(candidates) > maxApproxCandidates {
		candidates = candidates[:maxApproxCandidates]
	}
	return callResolution{candidates: candidates, resolution: ResolutionApprox, confidence: confidence, reason: reason}
}

// unknownCall returns an unresolved call site.
func unknownCall(reason string) callResolution {
	return callResolution{resolution: ResolutionUnknown, reason: reason}
}

// unwrapCallTarget removes parentheses and generic instantiations from a callee
// expression.
func unwrapCallTarget(expr ast.Expr) ast.Expr {
	for {
		switch typed := expr.(type) {
		case *ast.ParenExpr:
			expr = typed.X
		case *ast.IndexExpr:
			expr = typed.X
		case *ast.IndexListExpr:
			expr = typed.X
		default:
			return expr
		}
	}
}

// isInterfaceDispatch reports whether the receiver requires dynamic dispatch.
func isInterfaceDispatch(receiver types.Type) bool {
	if receiver == nil {
		return false
	}
	return types.IsInterface(receiver)
}

// resolveTypedCall binds a call expression using go/types information.
func resolveTypedCall(info *types.Info, call *ast.CallExpr, index symbolIndex) callResolution {
	target := unwrapCallTarget(call.Fun)
	switch fun := target.(type) {
	case *ast.Ident:
		object := info.Uses[fun]
		if object == nil {
			object = info.Defs[fun]
		}
		if object == nil {
			return unknownCall("call target is not a resolved identifier")
		}
		switch typed := object.(type) {
		case *types.Func:
			return exactCall(symbolIDForFunc(typed))
		case *types.Builtin:
			return callResolution{skip: true}
		case *types.TypeName:
			return callResolution{skip: true}
		case *types.Var:
			if _, ok := typed.Type().Underlying().(*types.Signature); ok {
				return unknownCall("call through a function value")
			}
			return unknownCall("call target is not callable")
		default:
			return unknownCall("call target has an unsupported object kind")
		}
	case *ast.SelectorExpr:
		if selection, ok := info.Selections[fun]; ok {
			switch selection.Kind() {
			case types.MethodVal, types.MethodExpr:
				method, ok := selection.Obj().(*types.Func)
				if !ok {
					return unknownCall("method selection has no function object")
				}
				if isInterfaceDispatch(selection.Recv()) {
					return approxCalls(index.methodNames[method.Name()], 0.2,
						fmt.Sprintf("interface dispatch to method %s resolved by name", method.Name()))
				}
				return exactCall(symbolIDForFunc(method))
			default:
				object := info.Uses[fun.Sel]
				if variable, ok := object.(*types.Var); ok {
					if _, isSignature := variable.Type().Underlying().(*types.Signature); isSignature {
						return unknownCall("call through a struct field function value")
					}
				}
				return unknownCall("selector call target is not a method")
			}
		}
		object := info.Uses[fun.Sel]
		if object == nil {
			return unknownCall("selector call target is unresolved")
		}
		switch typed := object.(type) {
		case *types.Func:
			return exactCall(symbolIDForFunc(typed))
		case *types.Builtin:
			return callResolution{skip: true}
		case *types.TypeName:
			return callResolution{skip: true}
		case *types.Var:
			if _, ok := typed.Type().Underlying().(*types.Signature); ok {
				return unknownCall("call through a function value")
			}
			return unknownCall("selector call target is not callable")
		default:
			return unknownCall("selector call target has an unsupported object kind")
		}
	default:
		return unknownCall("call target expression is not statically resolvable")
	}
}

// extractTypedCallGraph walks typed function bodies and emits call edges.
func extractTypedCallGraph(root string, pkgs []*packages.Package, fset *token.FileSet, index symbolIndex, limits *limitSet) ([]CallEdge, []Unresolved) {
	var edges []CallEdge
	var unresolved []Unresolved
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			relativeFile := relativeToRoot(root, fset.Position(file.Pos()).Filename)
			for _, declaration := range file.Decls {
				funcDecl, ok := declaration.(*ast.FuncDecl)
				if !ok || funcDecl.Name == nil || funcDecl.Body == nil {
					continue
				}
				object, ok := pkg.TypesInfo.Defs[funcDecl.Name].(*types.Func)
				if !ok {
					continue
				}
				callerID := symbolIDForFunc(object)
				if callerID == "" {
					continue
				}
				ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					line := fset.Position(call.Pos()).Line
					resolved := resolveTypedCall(pkg.TypesInfo, call, index)
					if resolved.skip {
						return true
					}
					switch resolved.resolution {
					case ResolutionExact:
						edges = append(edges, CallEdge{
							Caller:     callerID,
							Callee:     resolved.callee,
							Resolution: ResolutionExact,
							Confidence: 1.0,
							File:       relativeFile,
							Line:       line,
						})
					case ResolutionApprox:
						for _, candidate := range resolved.candidates {
							edges = append(edges, CallEdge{
								Caller:     callerID,
								Callee:     candidate,
								Resolution: ResolutionApprox,
								Confidence: resolved.confidence,
								File:       relativeFile,
								Line:       line,
							})
						}
					default:
						edges = append(edges, CallEdge{
							Caller:     callerID,
							Callee:     "",
							Resolution: ResolutionUnknown,
							Confidence: 0,
							File:       relativeFile,
							Line:       line,
						})
						unresolved = append(unresolved, Unresolved{
							FilePath: relativeFile,
							Line:     line,
							Reason:   resolved.reason,
						})
					}
					return true
				})
			}
		}
	}
	edges, edgesTruncated := finalizeCallEdges(edges)
	if edgesTruncated {
		limits.add(fmt.Sprintf("call edge table capped at %d entries", maxCallEdges))
	}
	var unresolvedTruncated bool
	unresolved, unresolvedTruncated = finalizeUnresolved(unresolved)
	if unresolvedTruncated {
		limits.add(fmt.Sprintf("unresolved call site table capped at %d entries", maxUnresolved))
	}
	return edges, unresolved
}

// extractSyntaxCallGraph emits conservative edges without type information.
func extractSyntaxCallGraph(root string, pkgs []syntaxPackage, fset *token.FileSet, index symbolIndex, limits *limitSet) ([]CallEdge, []Unresolved) {
	var edges []CallEdge
	var unresolved []Unresolved
	for _, pkg := range pkgs {
		for _, file := range pkg.files {
			relativeFile := relativeToRoot(root, fset.Position(file.Pos()).Filename)
			for _, declaration := range file.Decls {
				funcDecl, ok := declaration.(*ast.FuncDecl)
				if !ok || funcDecl.Name == nil || funcDecl.Body == nil {
					continue
				}
				callerID := pkg.pkgPath + "#" + funcDecl.Name.Name
				if funcDecl.Recv != nil && len(funcDecl.Recv.List) > 0 {
					if receiver := astReceiverName(funcDecl.Recv.List[0].Type); receiver != "" {
						callerID = pkg.pkgPath + "#" + receiver + "." + funcDecl.Name.Name
					}
				}
				ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					line := fset.Position(call.Pos()).Line
					target := unwrapCallTarget(call.Fun)
					var resolved callResolution
					switch fun := target.(type) {
					case *ast.Ident:
						if syntaxBuiltins[fun.Name] {
							return true
						}
						resolved = approxCalls(capStrings(index.funcNames[fun.Name], maxApproxCandidates), 0.4,
							"syntax fallback resolved call by function name")
						if len(resolved.candidates) == 0 {
							resolved = unknownCall("syntax fallback could not bind function call without type information")
						}
					case *ast.SelectorExpr:
						resolved = approxCalls(capStrings(index.methodNames[fun.Sel.Name], maxApproxCandidates), 0.3,
							"syntax fallback resolved method call by name")
						if len(resolved.candidates) == 0 {
							resolved = approxCalls(capStrings(index.funcNames[fun.Sel.Name], maxApproxCandidates), 0.25,
								"syntax fallback resolved qualified call by name")
						}
						if len(resolved.candidates) == 0 {
							resolved = unknownCall("syntax fallback could not bind selector call without type information")
						}
					default:
						resolved = unknownCall("syntax fallback could not resolve call target expression")
					}
					if resolved.skip {
						return true
					}
					if resolved.resolution == ResolutionApprox {
						for _, candidate := range resolved.candidates {
							edges = append(edges, CallEdge{
								Caller:     callerID,
								Callee:     candidate,
								Resolution: ResolutionApprox,
								Confidence: resolved.confidence,
								File:       relativeFile,
								Line:       line,
							})
						}
						return true
					}
					edges = append(edges, CallEdge{
						Caller:     callerID,
						Callee:     "",
						Resolution: ResolutionUnknown,
						Confidence: 0,
						File:       relativeFile,
						Line:       line,
					})
					unresolved = append(unresolved, Unresolved{
						FilePath: relativeFile,
						Line:     line,
						Reason:   resolved.reason,
					})
					return true
				})
			}
		}
	}
	edges, edgesTruncated := finalizeCallEdges(edges)
	if edgesTruncated {
		limits.add(fmt.Sprintf("call edge table capped at %d entries", maxCallEdges))
	}
	var unresolvedTruncated bool
	unresolved, unresolvedTruncated = finalizeUnresolved(unresolved)
	if unresolvedTruncated {
		limits.add(fmt.Sprintf("unresolved call site table capped at %d entries", maxUnresolved))
	}
	return edges, unresolved
}

// syntaxBuiltins are skipped during syntax-only call extraction.
var syntaxBuiltins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// capStrings returns at most limit values from a sorted slice.
func capStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

// finalizeCallEdges sorts, de-duplicates and caps call edges.
func finalizeCallEdges(edges []CallEdge) ([]CallEdge, bool) {
	sort.Slice(edges, func(i, j int) bool {
		left, right := edges[i], edges[j]
		if left.Caller != right.Caller {
			return left.Caller < right.Caller
		}
		if left.Callee != right.Callee {
			return left.Callee < right.Callee
		}
		if left.File != right.File {
			return left.File < right.File
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Resolution < right.Resolution
	})
	deduped := edges[:0]
	for _, edge := range edges {
		if len(deduped) > 0 && deduped[len(deduped)-1] == edge {
			continue
		}
		deduped = append(deduped, edge)
	}
	truncated := false
	if len(deduped) > maxCallEdges {
		deduped = deduped[:maxCallEdges]
		truncated = true
	}
	if deduped == nil {
		return []CallEdge{}, truncated
	}
	return deduped, truncated
}

// finalizeUnresolved sorts, de-duplicates and caps unresolved call sites.
func finalizeUnresolved(values []Unresolved) ([]Unresolved, bool) {
	sort.Slice(values, func(i, j int) bool {
		left, right := values[i], values[j]
		if left.FilePath != right.FilePath {
			return left.FilePath < right.FilePath
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Reason < right.Reason
	})
	deduped := values[:0]
	seen := make(map[Unresolved]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		deduped = append(deduped, value)
	}
	truncated := false
	if len(deduped) > maxUnresolved {
		deduped = deduped[:maxUnresolved]
		truncated = true
	}
	if deduped == nil {
		return []Unresolved{}, truncated
	}
	return deduped, truncated
}

// extractTypedDataEdges derives coarse value-flow edges from assignments and
// call arguments. The analysis is intentionally shallow and bounded.
func extractTypedDataEdges(root string, pkgs []*packages.Package, fset *token.FileSet, index symbolIndex, limits *limitSet) []DataEdge {
	var edges []DataEdge
	seen := make(map[DataEdge]struct{})
	dataCapped := false
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			relativeFile := relativeToRoot(root, fset.Position(file.Pos()).Filename)
			for _, declaration := range file.Decls {
				funcDecl, ok := declaration.(*ast.FuncDecl)
				if !ok || funcDecl.Name == nil || funcDecl.Body == nil {
					continue
				}
				object, ok := pkg.TypesInfo.Defs[funcDecl.Name].(*types.Func)
				if !ok {
					continue
				}
				callerID := symbolIDForFunc(object)
				if callerID == "" {
					continue
				}
				origins := make(map[types.Object]string)
				ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
					switch typed := node.(type) {
					case *ast.AssignStmt:
						for position, right := range typed.Rhs {
							producer := dataProducer(pkg.TypesInfo, right, origins, index)
							if producer == "" {
								continue
							}
							if position < len(typed.Lhs) {
								if ident, ok := typed.Lhs[position].(*ast.Ident); ok {
									if target := pkg.TypesInfo.ObjectOf(ident); target != nil {
										origins[target] = producer
									}
								}
							}
							edge := DataEdge{
								From: producer,
								To:   callerID,
								Kind: "RESULT_FLOW",
								File: relativeFile,
								Line: fset.Position(typed.Pos()).Line,
							}
							if _, ok := seen[edge]; !ok {
								if len(edges) >= maxDataEdges {
									dataCapped = true
								} else {
									seen[edge] = struct{}{}
									edges = append(edges, edge)
								}
							}
						}
					case *ast.CallExpr:
						resolved := resolveTypedCall(pkg.TypesInfo, typed, index)
						if resolved.skip || resolved.resolution != ResolutionExact || resolved.callee == "" {
							return true
						}
						for _, argument := range typed.Args {
							producer := dataProducer(pkg.TypesInfo, argument, origins, index)
							if producer == "" || producer == resolved.callee {
								continue
							}
							edge := DataEdge{
								From: producer,
								To:   resolved.callee,
								Kind: "ARGUMENT_FLOW",
								File: relativeFile,
								Line: fset.Position(typed.Pos()).Line,
							}
							if _, ok := seen[edge]; !ok {
								if len(edges) >= maxDataEdges {
									dataCapped = true
								} else {
									seen[edge] = struct{}{}
									edges = append(edges, edge)
								}
							}
						}
					}
					return true
				})
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		left, right := edges[i], edges[j]
		if left.From != right.From {
			return left.From < right.From
		}
		if left.To != right.To {
			return left.To < right.To
		}
		if left.File != right.File {
			return left.File < right.File
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Kind < right.Kind
	})
	if len(edges) > maxDataEdges {
		dataCapped = true
		edges = edges[:maxDataEdges]
	}
	if dataCapped {
		limits.add(fmt.Sprintf("data edge table capped at %d entries", maxDataEdges))
	}
	if edges == nil {
		return []DataEdge{}
	}
	return edges
}

// dataProducer returns a symbol ID that produced an expression, if known.
func dataProducer(info *types.Info, expr ast.Expr, origins map[types.Object]string, index symbolIndex) string {
	switch typed := unwrapCallTarget(expr).(type) {
	case *ast.Ident:
		object := info.ObjectOf(typed)
		if object == nil {
			return ""
		}
		if origin, ok := origins[object]; ok {
			return origin
		}
		if function, ok := object.(*types.Func); ok {
			return symbolIDForFunc(function)
		}
	case *ast.CallExpr:
		resolved := resolveTypedCall(info, typed, index)
		if resolved.resolution == ResolutionExact {
			return resolved.callee
		}
	}
	return ""
}
