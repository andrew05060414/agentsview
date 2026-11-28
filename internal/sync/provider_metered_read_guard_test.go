package sync

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const contentReadMeterAllowMarker = "contentreadmeter:allow"

func checkMeteredFingerprintFile(
	t *testing.T,
	fset *token.FileSet,
	file *ast.File,
	filename string,
) []string {
	t.Helper()
	commentLines := make(map[int]string)
	for _, group := range file.Comments {
		for _, c := range group.List {
			pos := fset.Position(c.Slash)
			commentLines[pos.Line] += " " + c.Text
		}
	}
	isWhitelisted := func(pos token.Position) bool {
		for _, line := range []int{pos.Line, pos.Line - 1} {
			text := strings.ToLower(commentLines[line])
			if strings.Contains(text, contentReadMeterAllowMarker) ||
				strings.Contains(text, "nolint:contentreadmeter") {
				return true
			}
		}
		return false
	}

	var violations []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		isFingerprintFunc := name == "Fingerprint" ||
			strings.Contains(name, "Fingerprint") ||
			strings.Contains(name, "CompositeHash")
		if !isFingerprintFunc {
			continue
		}

		hasMeterCall := false
		var unguarded []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok {
				if pkgIdent, ok := sel.X.(*ast.Ident); ok {
					switch {
					case pkgIdent.Name == "io" &&
						(sel.Sel.Name == "Copy" || sel.Sel.Name == "CopyBuffer" || sel.Sel.Name == "CopyN"):
						pos := fset.Position(call.Pos())
						if !isWhitelisted(pos) {
							unguarded = append(unguarded, fmt.Sprintf(
								"%s:%d: %s calls io.%s without recordContentRead or %s",
								filename, pos.Line, name, sel.Sel.Name, contentReadMeterAllowMarker,
							))
						}
					case name == "Fingerprint" && pkgIdent.Name == "os" && sel.Sel.Name == "ReadFile":
						pos := fset.Position(call.Pos())
						if !isWhitelisted(pos) {
							unguarded = append(unguarded, fmt.Sprintf(
								"%s:%d: %s calls os.ReadFile without ContentReadMeter or %s",
								filename, pos.Line, name, contentReadMeterAllowMarker,
							))
						}
					}
				}
			}
			if ident, ok := call.Fun.(*ast.Ident); ok {
				if ident.Name == "recordContentRead" || ident.Name == "hashJSONLSourceFileContext" {
					hasMeterCall = true
				}
			}
			return true
		})

		if !hasMeterCall {
			violations = append(violations, unguarded...)
		}
	}
	return violations
}

// TestProviderFingerprintPathsMeterAllReads uses go/parser to ensure governed
// fingerprint paths do not introduce bare io.Copy (or os.ReadFile in
// Fingerprint) bypassing ContentReadMeter.
func TestProviderFingerprintPathsMeterAllReads(t *testing.T) {
	parserDir := filepath.Join("..", "parser")
	globs := []string{
		"antigravity*.go",
		"grok_provider.go",
		"cursor*.go",
		"vscode_copilot*.go",
		"deepseek_harness*.go",
	}

	seen := make(map[string]struct{})
	var targets []string
	for _, pattern := range globs {
		matches, err := filepath.Glob(filepath.Join(parserDir, pattern))
		require.NoError(t, err)
		for _, match := range matches {
			if strings.HasSuffix(match, "_test.go") {
				continue
			}
			if _, ok := seen[match]; ok {
				continue
			}
			seen[match] = struct{}{}
			targets = append(targets, match)
		}
	}
	require.NotEmpty(t, targets, "must locate governed provider files under internal/parser")

	fset := token.NewFileSet()
	var violations []string
	for _, path := range targets {
		file, err := goparser.ParseFile(fset, path, nil, goparser.ParseComments)
		require.NoError(t, err)
		violations = append(violations, checkMeteredFingerprintFile(t, fset, file, path)...)
	}
	assert.Empty(t, violations, "unguarded fingerprint file reads found:\n%s", strings.Join(violations, "\n"))
}

func TestProviderMeteredReadGuardSelfCheck(t *testing.T) {
	fset := token.NewFileSet()

	badSrc := `package parser
import "io"
func (s testSet) Fingerprint() {
	_, _ = io.Copy(nil, nil)
}`
	badFile, err := goparser.ParseFile(fset, "bad.go", badSrc, goparser.ParseComments)
	require.NoError(t, err)
	assert.NotEmpty(t, checkMeteredFingerprintFile(t, fset, badFile, "bad.go"))

	meteredSrc := `package parser
import "io"
func addTestFingerprintPart() {
	n, _ := io.Copy(nil, nil)
	recordContentRead(nil, n)
}`
	meteredFile, err := goparser.ParseFile(fset, "metered.go", meteredSrc, goparser.ParseComments)
	require.NoError(t, err)
	assert.Empty(t, checkMeteredFingerprintFile(t, fset, meteredFile, "metered.go"))

	allowedSrc := `package parser
import "io"
func (s testSet) Fingerprint() {
	// contentreadmeter:allow fixed small header
	_, _ = io.Copy(nil, nil)
}`
	allowedFile, err := goparser.ParseFile(fset, "allowed.go", allowedSrc, goparser.ParseComments)
	require.NoError(t, err)
	assert.Empty(t, checkMeteredFingerprintFile(t, fset, allowedFile, "allowed.go"))
}
