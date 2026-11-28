package conformance

import (
	"reflect"
	"testing"

	"github.com/nhanvoanh/payment-sdk/connector"
)

// cloneRaw phải giữ MỌI trường của RawWebhook. Test điền từng trường bằng reflect, nên thêm trường mới vào
// RawWebhook mà quên sửa cloneRaw thì test đỏ; trường có kiểu chưa biết thì test đòi bổ sung cách điền.
func TestCloneRaw_PreservesEveryField(t *testing.T) {
	var in connector.RawWebhook
	v := reflect.ValueOf(&in).Elem()
	for i := range v.NumField() {
		f, name := v.Field(i), v.Type().Field(i).Name
		switch f.Interface().(type) {
		case string:
			f.SetString("v_" + name)
		case []byte:
			f.SetBytes([]byte("v_" + name))
		case map[string][]string:
			f.Set(reflect.ValueOf(map[string][]string{"k_" + name: {"a", "b"}}))
		default:
			t.Fatalf("RawWebhook.%s có kiểu %s chưa biết: thêm cách điền vào test này và sao chép nó trong cloneRaw", name, f.Type())
		}
	}
	out := cloneRaw(in)
	ov := reflect.ValueOf(out)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if !reflect.DeepEqual(v.Field(i).Interface(), ov.Field(i).Interface()) {
			t.Errorf("cloneRaw làm rơi hay đổi RawWebhook.%s: %#v, cần %#v", name, ov.Field(i).Interface(), v.Field(i).Interface())
		}
	}
	// Bản sao sâu: sửa bản gốc không ảnh hưởng bản sao.
	in.Body[0] = 'X'
	in.Headers["k_Headers"][0] = "X"
	in.Query["k_Query"][0] = "X"
	if out.Body[0] == 'X' || out.Headers["k_Headers"][0] == "X" || out.Query["k_Query"][0] == "X" {
		t.Error("cloneRaw không sao chép sâu Body/Headers/Query")
	}
}
