package bootstrap

import (
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/config"
)

func TestNewEnginesRegistersPlugins(t *testing.T) {
	cfg := config.Default()
	cfg.Execution.Plugins = []config.PluginConfig{{
		Name: "python-analyzer", Command: []string{"/bin/true"},
		Languages: []string{"python"}, StepKinds: []string{"analyze", "impact-analysis"},
		MaxFrameBytes: 1 << 20, Timeout: 30 * time.Second,
	}}
	registry, err := NewEngines(cfg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, capability := range registry.Capabilities() {
		if capability.Name != "python-analyzer" {
			continue
		}
		found = true
		if len(capability.Kinds) != 2 || capability.Kinds[0] != domain.StepKindAnalyze {
			t.Fatalf("unexpected kinds: %#v", capability.Kinds)
		}
		if len(capability.Languages) != 1 || capability.Languages[0] != "python" {
			t.Fatalf("unexpected languages: %#v", capability.Languages)
		}
	}
	if !found {
		t.Fatal("plugin engine was not registered")
	}
}

func TestNewEnginesRejectsReservedPluginName(t *testing.T) {
	cfg := config.Default()
	cfg.Execution.Plugins = []config.PluginConfig{{
		Name: "go-analyzer", Command: []string{"/bin/true"},
		Languages: []string{"go"}, StepKinds: []string{"analyze"},
	}}
	if _, err := NewEngines(cfg); err == nil {
		t.Fatal("reserved plugin name was accepted")
	}
}

func TestConfiguredCapabilitiesIncludePlugins(t *testing.T) {
	cfg := config.Default()
	cfg.Execution.AllowedLanguages = []string{"go", "python"}
	cfg.Execution.Plugins = []config.PluginConfig{{Name: "python-analyzer"}}
	languages := ConfiguredLanguages(cfg)
	engines := ConfiguredEngines(cfg)
	if len(languages) != 2 {
		t.Fatalf("unexpected languages: %#v", languages)
	}
	hasPlugin := false
	for _, engine := range engines {
		if engine == "python-analyzer" {
			hasPlugin = true
		}
	}
	if !hasPlugin {
		t.Fatalf("plugin missing from engines: %#v", engines)
	}
}
