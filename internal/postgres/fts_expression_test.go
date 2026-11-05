package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGFTSExpressionInputErrors(t *testing.T) {
	for _, query := range []string{`"a" OR`, `"a" ("b")`, `"reader" OR broken"`, `"reader" OR ""`} {
		t.Run(query, func(t *testing.T) {
			var pb paramBuilder
			_, err := pgFTSExpression(query, "m.content", &pb)
			var inputErr *db.SearchInputError
			require.ErrorAs(t, err, &inputErr)
		})
	}
}
