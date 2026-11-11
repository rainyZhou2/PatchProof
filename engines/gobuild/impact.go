package gobuild

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/impact"
)

const maxImpactBytes = 8 << 20

func applyImpactPatterns(request engine.Request, view optionsView, kind domain.StepKind) (optionsView, error) {
	if kind != domain.StepKindRegression || view.patternsExplicit {
		return view, nil
	}
	staged, ok := request.Staged["impact"]
	if !ok || strings.TrimSpace(staged.Path) == "" {
		return view, nil
	}
	path, err := filepath.Abs(staged.Path)
	if err != nil {
		return optionsView{}, fmt.Errorf("resolve impact input: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return optionsView{}, fmt.Errorf("resolve impact input symlinks: %w", err)
	}
	if root := strings.TrimSpace(request.Workspace); root != "" {
		absoluteRoot, err := filepath.Abs(root)
		if err != nil {
			return optionsView{}, fmt.Errorf("resolve workspace: %w", err)
		}
		resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot)
		if err != nil {
			return optionsView{}, fmt.Errorf("resolve workspace symlinks: %w", err)
		}
		if !isWithin(resolvedRoot, resolved) {
			return optionsView{}, fmt.Errorf("impact input is outside the step workspace")
		}
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return optionsView{}, fmt.Errorf("impact input is not a regular file")
	}
	if info.Size() > maxImpactBytes {
		return optionsView{}, fmt.Errorf("impact input exceeds %d bytes", maxImpactBytes)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return optionsView{}, fmt.Errorf("read impact input: %w", err)
	}
	var bundle impact.Bundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return optionsView{}, fmt.Errorf("decode impact input: %w", err)
	}
	if bundle.SchemaVersion != impact.SchemaVersion {
		return optionsView{}, fmt.Errorf("impact input schema %q is not %q", bundle.SchemaVersion, impact.SchemaVersion)
	}
	patterns := make([]string, 0, len(bundle.AffectedPackages))
	seen := make(map[string]struct{}, len(bundle.AffectedPackages))
	for _, pattern := range bundle.AffectedPackages {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if err := validatePattern(pattern); err != nil {
			continue
		}
		if _, duplicate := seen[pattern]; duplicate {
			continue
		}
		seen[pattern] = struct{}{}
		patterns = append(patterns, pattern)
		if len(patterns) >= maxPatterns {
			break
		}
	}
	if len(patterns) == 0 {
		return view, nil
	}
	view.patterns = patterns
	view.impactScoped = true
	return view, nil
}
