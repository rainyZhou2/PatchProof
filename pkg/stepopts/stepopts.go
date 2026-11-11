package stepopts

import (
	"strconv"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	ContractRef        = "contract.ref"
	ContractID         = "contract.id"
	ClaimID            = "claim.id"
	TargetSymbol       = "symbol"
	Language           = "language"
	Engine             = "engine"
	BaseSnapshot       = "base_snapshot"
	HeadSnapshot       = "head_snapshot"
	BaseCommit         = "base_commit"
	HeadCommit         = "head_commit"
	FuzzSeconds        = "fuzz.seconds"
	FuzzMaxCases       = "fuzz.max_cases"
	FuzzSeed           = "fuzz.seed"
	StepTimeoutSeconds = "timeout.seconds"
	ReportFormats      = "report.formats"
	ProtectedPaths     = "protected_paths"
	MinimumResult      = "minimum_result"
	InputDomain        = "input_domain"
	Observable         = "observable"
	CheckName          = "check.name"
	SkipTests          = "skip_tests"
	TestPattern        = "test.pattern"
	Goproxy            = "go.proxy"
	GoFlags            = "go.flags"
	ReplayInput        = "replay.input"
	ReplayCase         = "replay.case"
	ReplayCodec        = "replay.codec"
	InputPrefix        = "input."
)

func Get(options map[string]string, key string) string {
	if options == nil {
		return ""
	}
	return options[key]
}

func GetInt64(options map[string]string, key string, fallback int64) int64 {
	raw := Get(options, key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func GetBool(options map[string]string, key string, fallback bool) bool {
	raw := strings.ToLower(Get(options, key))
	switch raw {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return fallback
}

func SplitList(options map[string]string, key string) []string {
	raw := Get(options, key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func NamedInput(spec domain.StepSpec, name string) (domain.InputBinding, bool) {
	binding, ok := spec.NamedInputs[name]
	return binding, ok
}
