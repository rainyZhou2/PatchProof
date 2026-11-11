package godifferential

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/pkg/contract"
	evidenceutil "github.com/patchproof/patchproof/pkg/evidence"
)

const maxCorpusSize = 4096

// generator deterministically produces cases: boundary seeds and contract
// seeds first, then mutations of the growing corpus, capped by the case budget.
type generator struct {
	signature harness.Signature
	rng       *rand.Rand
	maxCases  int

	initial       []caseInput
	initialIndex  int
	corpus        [][]any
	seen          map[string]struct{}
	produced      int
	seedCount     int64
	invalidSeeds  int
	oversize      int
	exhausted     bool
	generationErr error
}

func newGenerator(signature harness.Signature, explicit []contract.Case, generators []string, maxCases int, seed int64) *generator {
	corpus := buildSeedCorpus(signature, explicit, maxCases, generators)
	g := &generator{
		signature:    signature,
		rng:          newRNG(seed),
		maxCases:     maxCases,
		initial:      corpus.cases,
		corpus:       corpus.corpus,
		seen:         make(map[string]struct{}, len(corpus.cases)),
		seedCount:    corpus.seedCount,
		invalidSeeds: corpus.invalidSeed,
		oversize:     corpus.oversize,
	}
	for _, item := range corpus.cases {
		g.seen[string(item.raw)] = struct{}{}
	}
	return g
}

// Next returns the next deterministic case or false when the budget or the
// generator is exhausted.
func (g *generator) Next() (caseInput, bool) {
	if g.produced >= g.maxCases {
		return caseInput{}, false
	}
	if g.initialIndex < len(g.initial) {
		item := g.initial[g.initialIndex]
		g.initialIndex++
		g.produced++
		return item, true
	}
	if len(g.corpus) == 0 || g.signature.Arity == 0 || g.exhausted {
		return caseInput{}, false
	}
	for attempt := 0; attempt < 64; attempt++ {
		base := g.corpus[g.rng.IntN(len(g.corpus))]
		args := cloneArgs(base)
		count := 1 + g.rng.IntN(3)
		for index := 0; index < count; index++ {
			slot := g.rng.IntN(len(args))
			args[slot] = g.mutate(g.signature.Params[slot], args[slot], 0)
		}
		candidate, ok := newCaseInput(args)
		if !ok {
			g.oversize++
			continue
		}
		key := string(candidate.raw)
		if _, exists := g.seen[key]; exists {
			continue
		}
		g.seen[key] = struct{}{}
		if len(g.corpus) < maxCorpusSize {
			g.corpus = append(g.corpus, candidate.args)
		}
		g.produced++
		return candidate, true
	}
	g.exhausted = true
	g.generationErr = fmt.Errorf("mutation generator could not produce a novel case")
	return caseInput{}, false
}

// summary records generator bookkeeping that belongs in evidence limits.
func (g *generator) summary() map[string]string {
	digest := ""
	if encoded, err := json.Marshal(g.signature); err == nil {
		digest = evidenceutil.DigestBytes(encoded)
	}
	return map[string]string{
		"seeds":          fmt.Sprintf("%d", g.seedCount),
		"generated":      fmt.Sprintf("%d", g.produced),
		"invalid_seeds":  fmt.Sprintf("%d", g.invalidSeeds),
		"oversize_cases": fmt.Sprintf("%d", g.oversize),
		"signature":      digest,
	}
}
