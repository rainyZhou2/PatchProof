package godifferential

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/pkg/contract"
	evidenceutil "github.com/patchproof/patchproof/pkg/evidence"
)

// caseInput is one fully encoded argument vector. raw is the canonical JSON
// representation used for CAS blobs, case identifiers and execution.
type caseInput struct {
	args []any
	raw  []byte
}

func newCaseInput(args []any) (caseInput, bool) {
	encoded, err := json.Marshal(args)
	if err != nil || len(encoded) > harness.MaxInputBytes {
		return caseInput{}, false
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return caseInput{}, false
	}
	canonical, err := evidenceutil.Canonicalize(sanitizeNegativeZero(decoded))
	if err != nil || len(canonical) > harness.MaxInputBytes {
		return caseInput{}, false
	}
	return caseInput{args: args, raw: canonical}, true
}

func (c caseInput) id() string {
	digest := evidenceutil.DigestBytes(c.raw)
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) > 32 {
		hex = hex[:32]
	}
	return "ce-" + hex
}

// sanitizeNegativeZero replaces -0 with 0 because the evidence canonicalizer
// rejects the raw "-0" number form.
func sanitizeNegativeZero(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if typed.String() == "-0" {
			return json.Number("0")
		}
		return typed
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = sanitizeNegativeZero(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = sanitizeNegativeZero(item)
		}
		return out
	default:
		return typed
	}
}

func numberInt(value int64) json.Number {
	return json.Number(strconv.FormatInt(value, 10))
}

func numberUint(value uint64) json.Number {
	return json.Number(strconv.FormatUint(value, 10))
}

func numberFloat(value float64, bits int) json.Number {
	if bits != 32 {
		bits = 64
	}
	return json.Number(strconv.FormatFloat(value, 'g', -1, bits))
}

func zeroValue(desc harness.TypeDesc) any {
	switch desc.Kind {
	case "bool":
		return false
	case "int", "int8", "int16", "int32", "int64":
		return numberInt(0)
	case "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return numberUint(0)
	case "float32", "float64":
		return numberFloat(0, desc.Bits)
	case "string":
		return ""
	case "struct":
		return map[string]any{}
	case "array":
		return []any{}
	default:
		return nil
	}
}

// nonNilValue returns a JSON value that unmarshals into a non-nil pointer or
// interface of the described type.
func nonNilValue(desc harness.TypeDesc, depth int) any {
	if depth > 4 {
		return nil
	}
	switch desc.Kind {
	case "ptr":
		if desc.Elem == nil {
			return nil
		}
		return nonNilValue(*desc.Elem, depth+1)
	case "slice":
		return []any{}
	case "map":
		return map[string]any{}
	case "interface":
		return numberInt(0)
	case "struct":
		return map[string]any{}
	case "array":
		return []any{}
	default:
		return zeroValue(desc)
	}
}

func mapKeySample(desc *harness.TypeDesc) (string, bool) {
	if desc == nil {
		return "", false
	}
	switch desc.Kind {
	case "string":
		return "k", true
	case "int", "int8", "int16", "int32", "int64":
		return "0", true
	case "uint", "uint8", "uint16", "uint32", "uint64":
		return "0", true
	case "bool":
		return "false", true
	default:
		return "", false
	}
}

func elementSample(desc *harness.TypeDesc, index int) any {
	if desc == nil {
		return nil
	}
	switch desc.Kind {
	case "string":
		if index%2 == 0 {
			return "a"
		}
		return ""
	case "int", "int8", "int16", "int32", "int64":
		return numberInt(int64(index % 3))
	case "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return numberUint(uint64(index % 3))
	case "float32", "float64":
		return numberFloat(float64(index%3), desc.Bits)
	case "bool":
		return index%2 == 0
	case "slice", "map", "ptr", "interface":
		return nil
	case "struct":
		return map[string]any{}
	case "array":
		return []any{}
	default:
		return nil
	}
}

func dedupeValues(values []any) []any {
	seen := make(map[string]struct{}, len(values))
	out := make([]any, 0, len(values))
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		if _, exists := seen[string(encoded)]; exists {
			continue
		}
		seen[string(encoded)] = struct{}{}
		out = append(out, value)
	}
	return out
}

// boundaryValues returns type-directed boundary arguments for one parameter.
// Columns are consumed round-robin so that every parameter receives its own
// boundary values without a combinatorial cross product.
func boundaryValues(desc harness.TypeDesc) []any {
	switch desc.Kind {
	case "bool":
		return []any{false, true}
	case "int", "int8", "int16", "int32", "int64":
		bits := desc.Bits
		if bits <= 0 || bits > 64 {
			bits = 64
		}
		minimum := -(int64(1) << (bits - 1))
		maximum := int64(1)<<(bits-1) - 1
		values := []any{
			numberInt(0), numberInt(1), numberInt(-1),
			numberInt(minimum), numberInt(maximum),
			numberInt(minimum + 1), numberInt(maximum - 1),
		}
		if desc.Name == "time.Duration" {
			values = append(values, numberInt(1_000_000_000), numberInt(-1_000_000_000), numberInt(60_000_000_000))
		}
		return dedupeValues(values)
	case "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		bits := desc.Bits
		if bits <= 0 || bits > 64 {
			bits = 64
		}
		maximum := uint64(1)<<uint(bits) - 1
		if bits == 64 {
			maximum = ^uint64(0)
		}
		return dedupeValues([]any{
			numberUint(0), numberUint(1), numberUint(maximum), numberUint(maximum - 1),
		})
	case "float32", "float64":
		bits := desc.Bits
		maximum := math.MaxFloat64
		tiny := math.SmallestNonzeroFloat64
		if bits == 32 {
			maximum = float64(math.MaxFloat32)
			tiny = float64(math.SmallestNonzeroFloat32)
		}
		return dedupeValues([]any{
			numberFloat(0, bits),
			numberFloat(1, bits),
			numberFloat(-1, bits),
			numberFloat(maximum, bits),
			numberFloat(-maximum, bits),
			numberFloat(tiny, bits),
			numberFloat(-tiny, bits),
			map[string]any{"__float__": "NaN"},
			map[string]any{"__float__": "+Inf"},
			map[string]any{"__float__": "-Inf"},
		})
	case "string":
		return []any{
			"", "a", "\x00", "héllo – 世界\n", "   ", strings.Repeat("a", 4096),
		}
	case "slice":
		if desc.Elem == nil {
			return []any{nil, []any{}, []any{nil}}
		}
		big := make([]any, 1024)
		for index := range big {
			big[index] = elementSample(desc.Elem, index)
		}
		return dedupeValues([]any{
			nil,
			[]any{},
			[]any{elementSample(desc.Elem, 0)},
			[]any{elementSample(desc.Elem, 0), elementSample(desc.Elem, 1)},
			big,
		})
	case "array":
		if desc.Elem == nil {
			return []any{[]any{}}
		}
		return dedupeValues([]any{[]any{}, []any{elementSample(desc.Elem, 0)}})
	case "map":
		if desc.Elem == nil {
			return []any{nil, map[string]any{}}
		}
		key, ok := mapKeySample(desc.Key)
		if !ok {
			return []any{nil, map[string]any{}}
		}
		return dedupeValues([]any{
			nil,
			map[string]any{},
			map[string]any{key: elementSample(desc.Elem, 0)},
		})
	case "ptr":
		if desc.Elem == nil {
			return []any{nil}
		}
		return dedupeValues([]any{nil, nonNilValue(*desc.Elem, 0)})
	case "struct":
		return []any{map[string]any{}}
	case "interface":
		return []any{nil}
	default:
		return []any{nil}
	}
}

func cloneArgs(args []any) []any {
	out := make([]any, len(args))
	copy(out, args)
	return out
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = cloneValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = cloneValue(item)
		}
		return out
	default:
		return typed
	}
}

func newRNG(seed int64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15))
}

// seedCorpus builds the deterministic seed corpus: an all-zero case, one
// boundary column per parameter, and the contract's explicit seeds.
type seedCorpus struct {
	cases       []caseInput
	corpus      [][]any
	seedCount   int64
	invalidSeed int
	oversize    int
}

func buildSeedCorpus(signature harness.Signature, explicit []contract.Case, maxCases int, generatorHints []string) seedCorpus {
	corpus := seedCorpus{}
	seen := make(map[string]struct{})
	zero := make([]any, signature.Arity)
	for index := range zero {
		zero[index] = zeroValue(signature.Params[index])
	}
	add := func(args []any) {
		if len(corpus.cases) >= maxCases {
			return
		}
		candidate, ok := newCaseInput(args)
		if !ok {
			corpus.oversize++
			return
		}
		key := string(candidate.raw)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		corpus.cases = append(corpus.cases, candidate)
		corpus.corpus = append(corpus.corpus, candidate.args)
	}
	add(cloneArgs(zero))

	hints := make(map[string]struct{}, len(generatorHints))
	for _, hint := range generatorHints {
		hints[strings.ToLower(strings.TrimSpace(hint))] = struct{}{}
	}
	boundaries := make([][]any, signature.Arity)
	maxBoundary := 0
	for index := range signature.Params {
		boundaries[index] = boundaryValues(signature.Params[index])
		if _, ok := hints["duration"]; ok {
			switch signature.Params[index].Kind {
			case "int", "int64":
				boundaries[index] = dedupeValues(append(boundaries[index],
					numberInt(1_000_000_000), numberInt(-1_000_000_000), numberInt(60_000_000_000)))
			}
		}
		if len(boundaries[index]) > maxBoundary {
			maxBoundary = len(boundaries[index])
		}
	}
	for round := 0; round < maxBoundary; round++ {
		for index := range signature.Params {
			if round >= len(boundaries[index]) {
				continue
			}
			args := cloneArgs(zero)
			args[index] = boundaries[index][round]
			add(args)
		}
	}

	for _, seed := range explicit {
		encoded, err := json.Marshal(seed.Args)
		if err != nil {
			corpus.invalidSeed++
			continue
		}
		args, err := decodeJSONArray(encoded)
		if err != nil || len(args) != signature.Arity {
			corpus.invalidSeed++
			continue
		}
		add(args)
	}
	corpus.seedCount = int64(len(corpus.cases))
	return corpus
}

func decodeJSONArray(raw []byte) ([]any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var args []any
	if err := decoder.Decode(&args); err != nil {
		return nil, err
	}
	return args, nil
}

func intValue(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func uintValue(value any) (uint64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseUint(number.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func floatValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(typed.String(), 64)
		return parsed, err == nil
	case map[string]any:
		switch typed["__float__"] {
		case "NaN":
			return math.NaN(), true
		case "+Inf":
			return math.Inf(1), true
		case "-Inf":
			return math.Inf(-1), true
		}
	}
	return 0, false
}

func clampInt(value int64, bits int) int64 {
	if bits <= 0 || bits >= 64 {
		return value
	}
	minimum := -(int64(1) << (bits - 1))
	maximum := int64(1)<<(bits-1) - 1
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func clampUint(value uint64, bits int) uint64 {
	if bits <= 0 || bits >= 64 {
		return value
	}
	maximum := uint64(1)<<uint(bits) - 1
	if value > maximum {
		return maximum
	}
	return value
}

func truncateRunes(text string, count int) string {
	if count <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= count {
		return text
	}
	return string(runes[:count])
}

// mutate produces a deterministic mutation of one argument value.
func (g *generator) mutate(desc harness.TypeDesc, current any, depth int) any {
	if depth > 3 {
		return cloneValue(current)
	}
	switch desc.Kind {
	case "bool":
		value, ok := current.(bool)
		if !ok {
			return true
		}
		return !value
	case "int", "int8", "int16", "int32", "int64":
		bits := desc.Bits
		value, ok := intValue(current)
		if !ok {
			value = 0
		}
		switch g.rng.IntN(7) {
		case 0:
			return numberInt(0)
		case 1:
			return numberInt(clampInt(value+1, bits))
		case 2:
			return numberInt(clampInt(value-1, bits))
		case 3:
			return numberInt(value / 2)
		case 4:
			return numberInt(clampInt(int64(g.rng.IntN(2001))-1000, bits))
		case 5:
			return numberInt(clampInt(value^(int64(1)<<uint(g.rng.IntN(maxInt(1, bits)))), bits))
		default:
			values := boundaryValues(desc)
			return values[g.rng.IntN(len(values))]
		}
	case "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		bits := desc.Bits
		value, ok := uintValue(current)
		if !ok {
			value = 0
		}
		switch g.rng.IntN(6) {
		case 0:
			return numberUint(0)
		case 1:
			return numberUint(clampUint(value+1, bits))
		case 2:
			return numberUint(value / 2)
		case 3:
			return numberUint(uint64(g.rng.IntN(2001)))
		case 4:
			return numberUint(clampUint(value^(uint64(1)<<uint(g.rng.IntN(maxInt(1, bits)))), bits))
		default:
			values := boundaryValues(desc)
			return values[g.rng.IntN(len(values))]
		}
	case "float32", "float64":
		bits := desc.Bits
		value, ok := floatValue(current)
		if !ok {
			value = 0
		}
		switch g.rng.IntN(7) {
		case 0:
			return numberFloat(0, bits)
		case 1:
			return numberFloat(value/2, bits)
		case 2:
			return numberFloat(value*2, bits)
		case 3:
			return numberFloat(float64(g.rng.IntN(21)-10)/4, bits)
		case 4:
			return map[string]any{"__float__": "NaN"}
		case 5:
			return map[string]any{"__float__": "+Inf"}
		default:
			values := boundaryValues(desc)
			return values[g.rng.IntN(len(values))]
		}
	case "string":
		text, ok := current.(string)
		if !ok {
			return "a"
		}
		switch g.rng.IntN(6) {
		case 0:
			return ""
		case 1:
			return truncateRunes(text, len([]rune(text))/2)
		case 2:
			if len(text) < 4096 {
				return text + "a"
			}
			return text
		case 3:
			if len([]rune(text)) < 2048 {
				return text + text
			}
			return text
		case 4:
			runes := []rune(text)
			if len(runes) == 0 {
				return "x"
			}
			runes[g.rng.IntN(len(runes))] = 'x'
			return string(runes)
		default:
			return strings.ReplaceAll(text, "a", "")
		}
	case "slice":
		if desc.Elem == nil {
			return nil
		}
		items, ok := current.([]any)
		if !ok {
			return []any{}
		}
		switch g.rng.IntN(5) {
		case 0:
			return nil
		case 1:
			if len(items) == 0 {
				return []any{}
			}
			return cloneValue(items[:len(items)/2])
		case 2:
			if len(items) >= 1024 {
				return items[:1023]
			}
			out := cloneValue(items).([]any)
			return append(out, g.randomValue(*desc.Elem, depth+1))
		case 3:
			if len(items) == 0 {
				return items
			}
			out := cloneValue(items).([]any)
			index := g.rng.IntN(len(out))
			return append(out[:index], out[index+1:]...)
		default:
			out := cloneValue(items).([]any)
			if len(out) > 0 {
				out[g.rng.IntN(len(out))] = g.randomValue(*desc.Elem, depth+1)
			}
			return out
		}
	case "array":
		if desc.Elem == nil {
			return []any{}
		}
		items, ok := current.([]any)
		if !ok || len(items) == 0 {
			return []any{g.randomValue(*desc.Elem, depth+1)}
		}
		out := cloneValue(items).([]any)
		out[g.rng.IntN(len(out))] = g.randomValue(*desc.Elem, depth+1)
		return out
	case "map":
		if desc.Elem == nil {
			return nil
		}
		key, ok := mapKeySample(desc.Key)
		if !ok {
			return nil
		}
		items, _ := current.(map[string]any)
		mutated := make(map[string]any, len(items)+1)
		for existingKey, item := range items {
			mutated[existingKey] = item
		}
		switch g.rng.IntN(4) {
		case 0:
			return nil
		case 1:
			delete(mutated, key)
			return mutated
		case 2:
			return map[string]any{}
		default:
			mutated[key] = g.randomValue(*desc.Elem, depth+1)
			return mutated
		}
	case "ptr":
		if desc.Elem == nil {
			return nil
		}
		if current == nil {
			return nonNilValue(*desc.Elem, depth+1)
		}
		if g.rng.IntN(4) == 0 {
			return nil
		}
		return g.mutate(*desc.Elem, current, depth+1)
	case "struct":
		return map[string]any{}
	case "interface":
		if g.rng.IntN(2) == 0 {
			return nil
		}
		return g.randomValue(harness.TypeDesc{Kind: "int64", Bits: 64}, depth+1)
	default:
		return cloneValue(current)
	}
}

func (g *generator) randomValue(desc harness.TypeDesc, depth int) any {
	if depth > 3 {
		return zeroValue(desc)
	}
	switch desc.Kind {
	case "bool":
		return g.rng.IntN(2) == 0
	case "int", "int8", "int16", "int32", "int64":
		return numberInt(clampInt(int64(g.rng.IntN(21))-10, desc.Bits))
	case "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return numberUint(clampUint(uint64(g.rng.IntN(21)), desc.Bits))
	case "float32", "float64":
		return numberFloat(float64(g.rng.IntN(21)-10)/4, desc.Bits)
	case "string":
		alphabet := []rune("abc\x00 -_")
		length := g.rng.IntN(8)
		var builder strings.Builder
		for index := 0; index < length; index++ {
			builder.WriteRune(alphabet[g.rng.IntN(len(alphabet))])
		}
		return builder.String()
	case "slice":
		if desc.Elem == nil {
			return nil
		}
		length := g.rng.IntN(4)
		out := make([]any, length)
		for index := range out {
			out[index] = g.randomValue(*desc.Elem, depth+1)
		}
		return out
	case "array":
		if desc.Elem == nil {
			return []any{}
		}
		return []any{g.randomValue(*desc.Elem, depth+1)}
	case "map":
		if desc.Elem == nil {
			return nil
		}
		key, ok := mapKeySample(desc.Key)
		if !ok {
			return nil
		}
		return map[string]any{key: g.randomValue(*desc.Elem, depth+1)}
	case "ptr":
		if desc.Elem == nil {
			return nil
		}
		if g.rng.IntN(2) == 0 {
			return nil
		}
		return nonNilValue(*desc.Elem, depth+1)
	case "struct":
		return map[string]any{}
	default:
		return nil
	}
}
