package sdk

// Luật cấu trúc của repo SDK, kiểm bằng cách đọc mã nguồn. Chúng từng nằm ở internal/archtest của repo core khi SDK
// còn là thư mục sdk/ trong đó; core không còn thấy mã nguồn SDK nên luật chuyển về đây.

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sdkModule = "github.com/nhanvoanh/payment-sdk"

type goFile struct {
	rel string // đường dẫn tương đối so với gốc repo, dùng dấu '/'
	ast *ast.File
}

// parseGoFiles đọc mọi file .go dưới sub (tương đối so với gốc repo), bỏ .git và thư mục có go.mod riêng (tools/).
func parseGoFiles(t *testing.T, sub string, keep func(rel string) bool) []goFile {
	t.Helper()
	var out []goFile
	err := filepath.WalkDir(sub, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if p != "." {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // module lồng, không thuộc module SDK
				}
			}
			return nil
		}
		rel := filepath.ToSlash(p)
		if !strings.HasSuffix(rel, ".go") || !keep(rel) {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		out = append(out, goFile{rel: rel, ast: f})
		return nil
	})
	require.NoError(t, err)
	return out
}

func isTest(rel string) bool              { return strings.HasSuffix(rel, "_test.go") }
func nonTest(rel string) bool             { return !isTest(rel) }
func all(string) bool                     { return true }
func importPath(s *ast.ImportSpec) string { p, _ := strconv.Unquote(s.Path.Value); return p }
func isStdlib(p string) bool              { return !strings.Contains(strings.SplitN(p, "/", 2)[0], ".") }

// requiredModules đọc các module trong khối require của go.mod (cả dòng require đơn) và báo go.mod có replace không.
func requiredModules(t *testing.T) (mods []string, hasReplace bool) {
	t.Helper()
	f, err := os.Open("go.mod")
	require.NoError(t, err)
	defer f.Close()
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "replace"):
			hasReplace = true
		case line == "require (":
			in = true
		case in && line == ")":
			in = false
		case in && line != "" && !strings.HasPrefix(line, "//"):
			mods = append(mods, strings.Fields(line)[0])
		case strings.HasPrefix(line, "require "):
			mods = append(mods, strings.Fields(line)[1])
		}
	}
	require.NoError(t, sc.Err())
	return mods, hasReplace
}

// TestSDKImportsOnlyDeclaredModules: mọi import (kể cả test) là thư viện chuẩn, chính module SDK, hoặc gói của một
// module khai trong go.mod; go.mod không có replace. Nhờ vậy SDK không bao giờ import core (core không có trong go.mod)
// và không dựng được từ một bản sao cục bộ lệch với phiên bản đã khai.
func TestSDKImportsOnlyDeclaredModules(t *testing.T) {
	mods, hasReplace := requiredModules(t)
	require.NotEmpty(t, mods)
	assert.False(t, hasReplace, "go.mod của SDK không được có replace")
	declared := func(p string) bool {
		if isStdlib(p) || p == sdkModule || strings.HasPrefix(p, sdkModule+"/") {
			return true
		}
		for _, m := range mods {
			if p == m || strings.HasPrefix(p, m+"/") {
				return true
			}
		}
		return false
	}
	files := parseGoFiles(t, ".", all)
	require.NotEmpty(t, files)
	var bad []string
	for _, f := range files {
		for _, imp := range f.ast.Imports {
			if p := importPath(imp); !declared(p) {
				bad = append(bad, fmt.Sprintf("%s: import %s", f.rel, p))
			}
		}
	}
	assert.Empty(t, bad, "SDK chỉ import thư viện chuẩn, chính nó và module khai trong go.mod:\n%s", strings.Join(bad, "\n"))
}

// TestSDKRedactIsStdlibOnly: gói che dữ liệu khách mà cả core lẫn connector dùng chỉ được dùng thư viện chuẩn
// (tầng app của core import nó; không dependency mới, không x/text).
func TestSDKRedactIsStdlibOnly(t *testing.T) {
	files := parseGoFiles(t, "redact", nonTest)
	require.NotEmpty(t, files)
	var bad []string
	for _, f := range files {
		for _, imp := range f.ast.Imports {
			if p := importPath(imp); !isStdlib(p) {
				bad = append(bad, fmt.Sprintf("%s: import %s", f.rel, p))
			}
		}
	}
	assert.Empty(t, bad, "redact chỉ được dùng thư viện chuẩn:\n%s", strings.Join(bad, "\n"))
}

// testKits là các gói bộ-test (file thường nhưng chạy dưới testing.TB, có kênh không mã hoá, CA tạm, đối tác giả):
// chỉ file _test.go và chính các gói này được import chúng, để không bước nhảy nào đưa chúng vào mã chạy thật. Khoá
// là thư mục tương đối gốc repo. Giữ khớp với rule depguard "test-kits" trong .golangci.yml; core giữ luật tương ứng
// cho mã của nó (internal/archtest).
var testKits = []string{"conformance", "connectorrpc/grpctest", "connectorrpc/tlstest", "connectortest",
	"template/acme/acmetest", "oteltrace/oteltracetest"}

// testKitUsers là các gói KHÔNG phải bộ-test nhưng được import ĐÚNG những bộ-test ghi kèm dù là file thường, mỗi
// dòng một ngoại lệ hẹp có lý do: template/cmd/fakepartner chạy đối tác giả của mẫu (acmetest) thành tiến trình cho
// compose của người phát triển (image riêng, không bao giờ nằm trong image core hay connector); bộ-test khác vào đó
// vẫn là vi phạm. Giữ khớp với rule depguard "test-kits" và "fakepartner-kits" trong .golangci.yml.
var testKitUsers = map[string][]string{"template/cmd/fakepartner": {"template/acme/acmetest"}}

func isTestKit(p string) bool {
	for _, k := range testKits {
		if p == sdkModule+"/"+k {
			return true
		}
	}
	return false
}

func inTestKit(rel string) bool {
	for _, k := range testKits {
		if path.Dir(rel) == k {
			return true
		}
	}
	return false
}

func allowedFor(rel, p string) bool {
	for _, k := range testKitUsers[path.Dir(rel)] {
		if p == sdkModule+"/"+k {
			return true
		}
	}
	return false
}

func testKitViolations(files []goFile) []string {
	var bad []string
	for _, f := range files {
		if isTest(f.rel) || inTestKit(f.rel) {
			continue
		}
		for _, imp := range f.ast.Imports {
			if p := importPath(imp); isTestKit(p) && !allowedFor(f.rel, p) {
				bad = append(bad, fmt.Sprintf("%s: import %s", f.rel, p))
			}
		}
	}
	return bad
}

func TestTestKitViolationsDetector(t *testing.T) {
	parse := func(rel, imp string) goFile {
		f, err := parser.ParseFile(token.NewFileSet(), rel, "package x\nimport _ \""+imp+"\"\n", 0)
		require.NoError(t, err)
		return goFile{rel: rel, ast: f}
	}
	conf := sdkModule + "/conformance"
	acmetest := sdkModule + "/template/acme/acmetest"
	got := testKitViolations([]goFile{
		parse("template/acme/wire.go", conf), // bước nhảy qua conformance
		parse("connectorsdk/x.go", sdkModule+"/connectorrpc/tlstest"),
		parse("providerhttp/x.go", sdkModule+"/connectortest"),
		parse("connectorsdk/x_test.go", conf),           // test: được
		parse("template/acme/acmetest/partner.go", conf), // bộ-test: được
		parse("conformance/harness.go", sdkModule+"/connectorrpc/grpctest"),
		parse("conformance/sub/x.go", sdkModule+"/connectorrpc/grpctest"),  // gói con không phải bộ-test
		parse("template/cmd/fakepartner/main.go", acmetest),                // ngoại lệ hẹp: được
		parse("template/cmd/fakepartner/main.go", conf),                    // ngoại lệ chỉ cho acmetest: không
		parse("template/cmd/fakepartner/x.go", sdkModule+"/connectortest"), // ngoại lệ chỉ cho acmetest: không
		parse("template/cmd/fakepartner/sub/x.go", acmetest),               // thư mục con của ngoại lệ: không
		parse("template/cmd/connector/main.go", acmetest),                  // connector thật: không
	})
	assert.ElementsMatch(t, []string{
		"template/acme/wire.go: import " + conf,
		"connectorsdk/x.go: import " + sdkModule + "/connectorrpc/tlstest",
		"providerhttp/x.go: import " + sdkModule + "/connectortest",
		"conformance/sub/x.go: import " + sdkModule + "/connectorrpc/grpctest",
		"template/cmd/fakepartner/main.go: import " + conf,
		"template/cmd/fakepartner/x.go: import " + sdkModule + "/connectortest",
		"template/cmd/fakepartner/sub/x.go: import " + acmetest,
		"template/cmd/connector/main.go: import " + acmetest,
	}, got)
}

func TestTestKitsOnlyInTests(t *testing.T) {
	files := parseGoFiles(t, ".", nonTest)
	require.NotEmpty(t, files)
	bad := testKitViolations(files)
	assert.Empty(t, bad, "gói bộ-test chỉ dùng trong file _test.go hoặc trong bộ-test khác:\n%s", strings.Join(bad, "\n"))
}
