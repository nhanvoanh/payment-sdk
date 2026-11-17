package sdk

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestForbiddenNames quét mọi file văn bản dưới sdk/ tìm các từ khoá trong SDK_FORBIDDEN_NAMES (phẩy,
// không phân biệt hoa thường). Danh sách do CI của repo sở hữu đặt vào, không nằm trong mã nguồn.
// Thấy thì chỉ in ĐƯỜNG DẪN, không in từ khoá: log CI không được làm lộ danh sách.
func TestForbiddenNames(t *testing.T) {
	var words [][]byte
	for _, w := range strings.Split(os.Getenv("SDK_FORBIDDEN_NAMES"), ",") {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, []byte(strings.ToLower(w)))
		}
	}
	if len(words) == 0 {
		t.Skip("SDK_FORBIDDEN_NAMES rỗng")
	}
	pathHits := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.sum" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 { // file nhị phân
			return nil
		}
		low := bytes.ToLower(b)
		lowPath := []byte(strings.ToLower(path))
		for _, w := range words {
			if bytes.Contains(lowPath, w) {
				// Từ khoá nằm ngay trong đường dẫn: in đường dẫn là lộ từ khoá ra log CI.
				pathHits++
				t.Errorf("một đường dẫn chứa từ khoá bị cấm (đường dẫn không được in, mục #%d)", pathHits)
				break
			}
			if bytes.Contains(low, w) {
				t.Errorf("%s chứa một từ khoá bị cấm", path)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
