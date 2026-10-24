package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPGFTSExpression(t *testing.T) {
	for _, tc := range []struct {
		name, query, operator string
		args                  []any
	}{
		{"implicit AND", "quick fox", " AND ", []any{"quick", "fox"}},
		{"Chinese OR", `"pxread" OR "压缩"`, " OR ", []any{"pxread", "压缩"}},
		{"Chinese short", "库", "ILIKE", []any{"库"}},
		{"quoted operator", `"OR"`, "ILIKE", []any{"OR"}},
		{"literal operator in plain input", "alpha OR beta", " AND ", []any{"alpha", "OR", "beta"}},
		{"phrase", `"quick fox"`, "ILIKE", []any{"quick fox"}},
		{"escaping", `"50%_" OR "a""b"`, " OR ", []any{`50\%\_`, `a"b`}},
		{"negative", `"alpha" NOT "beta"`, " AND NOT ", []any{"alpha", "beta"}},
		{"group", `"alpha" AND ("beta" OR "gamma")`, " OR ", []any{"alpha", "beta", "gamma"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pb := &paramBuilder{n: 2}
			query, err := pgFTSExpression(tc.query, pb)
			require.NoError(t, err)
			assert.Contains(t, query, tc.operator)
			assert.Equal(t, tc.args, pb.args)
			assert.Contains(t, query, "$3")
		})
	}
	for _, query := range []string{`"a" OR`, `"a" AND AND "b"`, `"unclosed`, `"a" OR ()`, `"a" NEAR "b"`, `"a" )`} {
		_, err := pgFTSExpression(query, &paramBuilder{})
		require.Error(t, err, query)
	}
}
