package goanalyzer

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/tools/go/packages"
)

// symbolIndex is the queryable form of the symbol table.
type symbolIndex struct {
	symbols     []Symbol
	byID        map[string]Symbol
	byFile      map[string][]string
	methodNames map[string][]string
	funcNames   map[string][]string
}

// buildSymbolIndex sorts symbols by ID and derives lookup tables.
func buildSymbolIndex(symbols []Symbol) symbolIndex {
	sorted := append([]Symbol(nil), symbols...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}
		if sorted[i].File != sorted[j].File {
			return sorted[i].File < sorted[j].File
		}
		return sorted[i].Line < sorted[j].Line
	})
	deduped := sorted[:0]
	seen := make(map[string]struct{}, len(sorted))
	for _, symbol := range sorted {
		if symbol.ID == "" {
			continue
		}
		if _, ok := seen[symbol.ID]; ok {
			continue
		}
		seen[symbol.ID] = struct{}{}
		deduped = append(deduped, symbol)
	}
	index := symbolIndex{
		symbols:     deduped,
		byID:        make(map[string]Symbol, len(deduped)),
		byFile:      make(map[string][]string),
		methodNames: make(map[string][]string),
		funcNames:   make(map[string][]string),
	}
	for _, symbol := range deduped {
		index.byID[symbol.ID] = symbol
		index.byFile[symbol.File] = append(index.byFile[symbol.File], symbol.ID)
		switch symbol.Kind {
		case SymbolKindMethod:
			index.methodNames[symbol.Name] = append(index.methodNames[symbol.Name], symbol.ID)
		case SymbolKindFunc:
			index.funcNames[symbol.Name] = append(index.funcNames[symbol.Name], symbol.ID)
		}
	}
	for file := range index.byFile {
		sort.Strings(index.byFile[file])
	}
	for name := range index.methodNames {
		sort.Strings(index.methodNames[name])
	}
	for name := range index.funcNames {
		sort.Strings(index.funcNames[name])
	}
	return index
}

// mergeSymbolIndexes merges head and base symbols, preferring head definitions.
func mergeSymbolIndexes(head, base symbolIndex) symbolIndex {
	merged := make([]Symbol, 0, len(head.symbols)+len(base.symbols))
	merged = append(merged, head.symbols...)
	seen := make(map[string]struct{}, len(head.symbols))
	for _, symbol := range head.symbols {
		seen[symbol.ID] = struct{}{}
	}
	for _, symbol := range base.symbols {
		if _, ok := seen[symbol.ID]; ok {
			continue
		}
		seen[symbol.ID] = struct{}{}
		merged = append(merged, symbol)
	}
	return buildSymbolIndex(merged)
}

// receiverTypeName returns the bare named type of a method receiver.
func receiverTypeName(receiver types.Type) string {
	current := receiver
	for {
		switch typed := current.(type) {
		case *types.Pointer:
			current = typed.Elem()
		case *types.Named:
			if typed.Obj() != nil {
				return typed.Obj().Name()
			}
			return ""
		case *types.Alias:
			current = types.Unalias(typed)
		default:
			text := types.TypeString(current, func(p *types.Package) string { return p.Path() })
			text = strings.TrimPrefix(text, "*")
			if idx := strings.Index(text, "["); idx >= 0 {
				text = text[:idx]
			}
			if idx := strings.LastIndex(text, "."); idx >= 0 {
				text = text[idx+1:]
			}
			return text
		}
	}
}

// symbolIDForFunc builds the stable package-qualified function or method ID.
func symbolIDForFunc(fn *types.Func) string {
	if fn == nil || fn.Pkg() == nil {
		return ""
	}
	signature, ok := fn.Type().(*types.Signature)
	if !ok {
		return ""
	}
	if signature.Recv() == nil {
		return fn.Pkg().Path() + "#" + fn.Name()
	}
	receiver := receiverTypeName(signature.Recv().Type())
	if receiver == "" {
		return fn.Pkg().Path() + "#" + fn.Name()
	}
	return fn.Pkg().Path() + "#" + receiver + "." + fn.Name()
}

// symbolIDForObject builds an ID for a non-function object.
func symbolIDForObject(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	return obj.Pkg().Path() + "#" + obj.Name()
}

// typeQualifier renders package references with full import paths so symbols
// are stable across checkouts.
func typeQualifier(pkg *types.Package) string {
	if pkg == nil {
		return ""
	}
	return pkg.Path()
}

// normalizeSignature collapses whitespace and bounds the signature length.
func normalizeSignature(raw string) string {
	raw = strings.ReplaceAll(raw, "\r", " ")
	raw = strings.ReplaceAll(raw, "\n", " ")
	raw = strings.ReplaceAll(raw, "\t", " ")
	raw = strings.Join(strings.Fields(raw), " ")
	if len(raw) > maxSignatureBytes {
		raw = raw[:maxSignatureBytes-3] + "..."
	}
	for len(raw) > 0 && !utf8.ValidString(raw) {
		raw = raw[:len(raw)-1]
	}
	return raw
}

// relativeToRoot converts an absolute filename into a snapshot-relative path.
func relativeToRoot(root, filename string) string {
	if filename == "" {
		return ""
	}
	if filepath.IsAbs(filename) {
		if relative, err := filepath.Rel(root, filename); err == nil && !strings.HasPrefix(relative, "..") {
			return filepath.ToSlash(relative)
		}
	}
	return filepath.ToSlash(filename)
}

// extractTypedSymbols builds a symbol index from fully type-checked packages.
func extractTypedSymbols(root string, pkgs []*packages.Package, fset *token.FileSet, limits *limitSet) symbolIndex {
	var symbols []Symbol
	for _, pkg := range pkgs {
		if pkg.Types == nil || pkg.TypesInfo == nil {
			continue
		}
		packageScope := pkg.Types.Scope()
		for _, obj := range pkg.TypesInfo.Defs {
			if obj == nil || obj.Pkg() == nil || obj.Pkg() != pkg.Types {
				continue
			}
			if obj.Name() == "_" || obj.Name() == "" {
				continue
			}
			symbol, ok := typedObjectSymbol(root, fset, obj, packageScope)
			if !ok {
				continue
			}
			symbols = append(symbols, symbol)
		}
	}
	symbols, truncated := capSymbols(symbols)
	if truncated {
		limits.add(fmt.Sprintf("symbol table capped at %d entries", maxSymbols))
	}
	return buildSymbolIndex(symbols)
}

// capSymbols deterministically sorts symbols before applying the size cap.
func capSymbols(symbols []Symbol) ([]Symbol, bool) {
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].ID != symbols[j].ID {
			return symbols[i].ID < symbols[j].ID
		}
		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}
		return symbols[i].Line < symbols[j].Line
	})
	if len(symbols) > maxSymbols {
		return symbols[:maxSymbols], true
	}
	return symbols, false
}

// typedObjectSymbol converts one go/types definition into an IR symbol.
func typedObjectSymbol(root string, fset *token.FileSet, obj types.Object, packageScope *types.Scope) (Symbol, bool) {
	symbol := Symbol{
		ID:          symbolIDForObject(obj),
		PackagePath: obj.Pkg().Path(),
		Name:        obj.Name(),
		File:        relativeToRoot(root, fset.Position(obj.Pos()).Filename),
		Line:        fset.Position(obj.Pos()).Line,
		Exported:    obj.Exported(),
	}
	switch typed := obj.(type) {
	case *types.Func:
		symbol.Kind = SymbolKindFunc
		if signature, ok := typed.Type().(*types.Signature); ok && signature.Recv() != nil {
			symbol.Kind = SymbolKindMethod
			symbol.Name = typed.Name()
		}
		symbol.ID = symbolIDForFunc(typed)
	case *types.TypeName:
		if obj.Parent() != packageScope {
			return Symbol{}, false
		}
		symbol.Kind = SymbolKindType
	case *types.Var:
		if obj.Parent() != packageScope {
			return Symbol{}, false
		}
		symbol.Kind = SymbolKindVar
	case *types.Const:
		if obj.Parent() != packageScope {
			return Symbol{}, false
		}
		symbol.Kind = SymbolKindConst
	default:
		return Symbol{}, false
	}
	if symbol.ID == "" {
		return Symbol{}, false
	}
	symbol.Signature = normalizeSignature(types.ObjectString(obj, typeQualifier))
	return symbol, true
}

// astReceiverName derives the receiver type name from a receiver expression.
func astReceiverName(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	text := types.ExprString(expr)
	text = strings.TrimPrefix(text, "*")
	if idx := strings.Index(text, "["); idx >= 0 {
		text = text[:idx]
	}
	if idx := strings.LastIndex(text, "."); idx >= 0 {
		text = text[idx+1:]
	}
	return strings.TrimSpace(text)
}

// printNode renders an AST node deterministically.
func printNode(fset *token.FileSet, node any) string {
	var buffer bytes.Buffer
	if err := printer.Fprint(&buffer, fset, node); err != nil {
		return ""
	}
	return buffer.String()
}

// astFuncSignature reconstructs a function or method signature from syntax.
func astFuncSignature(fset *token.FileSet, declaration *ast.FuncDecl) string {
	base := printNode(fset, declaration.Type)
	if declaration.Recv == nil || len(declaration.Recv.List) == 0 {
		return normalizeSignature(base)
	}
	receiver := astReceiverName(declaration.Recv.List[0].Type)
	rest := strings.TrimSpace(strings.TrimPrefix(base, "func"))
	return normalizeSignature("func (" + receiver + ") " + rest)
}

// astTypeSignature reconstructs a type declaration signature from syntax.
func astTypeSignature(fset *token.FileSet, specification *ast.TypeSpec) string {
	return normalizeSignature("type " + specification.Name.Name + " " + printNode(fset, specification.Type))
}

// astValueSignature reconstructs a var or const declaration signature.
func astValueSignature(tok token.Token, names []string, typ ast.Expr) string {
	prefix := "var"
	if tok == token.CONST {
		prefix = "const"
	}
	joined := strings.Join(names, ", ")
	if typ == nil {
		return normalizeSignature(prefix + " " + joined)
	}
	return normalizeSignature(prefix + " " + joined + " " + types.ExprString(typ))
}

// extractSyntaxSymbols builds a symbol index from syntax-only packages.
func extractSyntaxSymbols(root string, pkgs []syntaxPackage, fset *token.FileSet, limits *limitSet) symbolIndex {
	var symbols []Symbol
	for _, pkg := range pkgs {
		for _, file := range pkg.files {
			filename := relativeToRoot(root, fset.Position(file.Pos()).Filename)
			for _, declaration := range file.Decls {
				switch node := declaration.(type) {
				case *ast.FuncDecl:
					if node.Name == nil || node.Name.Name == "_" {
						continue
					}
					kind := SymbolKindFunc
					id := pkg.pkgPath + "#" + node.Name.Name
					if node.Recv != nil && len(node.Recv.List) > 0 {
						receiver := astReceiverName(node.Recv.List[0].Type)
						if receiver != "" {
							kind = SymbolKindMethod
							id = pkg.pkgPath + "#" + receiver + "." + node.Name.Name
						}
					}
					symbols = append(symbols, Symbol{
						ID:          id,
						PackagePath: pkg.pkgPath,
						Name:        node.Name.Name,
						Kind:        kind,
						Signature:   astFuncSignature(fset, node),
						File:        filename,
						Line:        fset.Position(node.Pos()).Line,
						Exported:    node.Name.IsExported(),
					})
				case *ast.GenDecl:
					for _, specification := range node.Specs {
						switch spec := specification.(type) {
						case *ast.TypeSpec:
							if spec.Name == nil || spec.Name.Name == "_" {
								continue
							}
							symbols = append(symbols, Symbol{
								ID:          pkg.pkgPath + "#" + spec.Name.Name,
								PackagePath: pkg.pkgPath,
								Name:        spec.Name.Name,
								Kind:        SymbolKindType,
								Signature:   astTypeSignature(fset, spec),
								File:        filename,
								Line:        fset.Position(spec.Pos()).Line,
								Exported:    spec.Name.IsExported(),
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
								symbols = append(symbols, Symbol{
									ID:          pkg.pkgPath + "#" + name.Name,
									PackagePath: pkg.pkgPath,
									Name:        name.Name,
									Kind:        kind,
									Signature:   signature,
									File:        filename,
									Line:        fset.Position(name.Pos()).Line,
									Exported:    name.IsExported(),
								})
							}
						}
					}
				}
			}
		}
	}
	symbols, truncated := capSymbols(symbols)
	if truncated {
		limits.add(fmt.Sprintf("symbol table capped at %d entries", maxSymbols))
	}
	return buildSymbolIndex(symbols)
}

// symbolsForFile returns the sorted symbol IDs declared in a file.
func (index symbolIndex) symbolsForFile(file string) []string {
	ids := index.byFile[file]
	out := append([]string(nil), ids...)
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}
