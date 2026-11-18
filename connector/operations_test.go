package connector

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mọi hằng kiểu Operation khai trong gói phải nằm trong AllOperations: thêm hằng mà quên danh sách thì
// Describe không khai thao tác đó và conformance không kiểm nó.
func TestAllOperations_ContainsEveryConstant(t *testing.T) {
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", nil, 0)
	require.NoError(t, err)
	var declared []string
	for _, f := range pkgs["connector"].Files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Operation" {
					continue
				}
				for _, v := range vs.Values {
					s, err := strconv.Unquote(v.(*ast.BasicLit).Value)
					require.NoError(t, err)
					declared = append(declared, s)
				}
			}
		}
	}
	var listed []string
	for _, op := range AllOperations() {
		listed = append(listed, string(op))
	}
	require.NotEmpty(t, declared)
	sort.Strings(declared)
	sort.Strings(listed)
	assert.Equal(t, declared, listed)
}

func TestAllOperations_ReturnsCopy(t *testing.T) {
	a := AllOperations()
	a[0] = "khác"
	assert.NotEqual(t, Operation("khác"), AllOperations()[0])
}
