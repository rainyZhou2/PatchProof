package json

import (
	"context"
	"encoding/json"

	"github.com/patchproof/patchproof/internal/domain"
)

type Reporter struct{}

func New() *Reporter { return &Reporter{} }

func (*Reporter) Format() string      { return "json" }
func (*Reporter) ContentType() string { return "application/json" }

func (*Reporter) Render(_ context.Context, report domain.Report, _ []domain.Evidence) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
