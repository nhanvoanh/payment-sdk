package redact_test

// Bộ sinh thân ngẫu nhiên có dữ liệu khách cho phép thử "phải che" (differential_test.go). Không import redact: file
// này cũng được chép nguyên vào công cụ so vi sai với bản cũ (xem báo cáo Task 6, Fix round 3).

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
)

type genCustomer struct{ Name, Email, Phone, Address string }

// genSecret là một giá trị khách đã chèn vào thân, ở dạng đã mã hoá đúng như nằm trong thân.
type genSecret struct {
	Kind    string // loại (xem genKinds)
	Encoded string
}

type genBody struct {
	Customer genCustomer
	Body     string
	Secrets  []genSecret
}

var (
	genSurnames = []string{"Nguyễn", "Trần", "Lê", "Phạm", "Huỳnh", "Hoàng", "Đặng", "Võ", "Đoàn", "Bùi"}
	genMiddles  = []string{"Văn", "Thị", "Minh", "Ngọc", "Thanh", "Quốc", "Hữu"}
	genGivens   = []string{"An", "Long", "Hoa", "Khai", "Tuấn", "Bình", "Thảo", "Dũng", "Yến", "Phúc"}
	genStreets  = []string{"Lê Lợi", "Nguyễn Huệ", "Trần Hưng Đạo", "Hai Bà Trưng", "Điện Biên Phủ"}
	genVillages = []string{"Thôn Đông", "Thôn Tây", "Ấp Bắc", "Xóm Mới"}
	genCommunes = []string{"Xã Tân Phú", "Xã An Bình", "Phường Bến Nghé", "Phường Đa Kao"}
	genDistrict = []string{"Huyện Củ Chi", "Quận 1", "Quận Ba Đình", "Huyện Đông Anh"}
	genCities   = []string{"TP Hồ Chí Minh", "Hà Nội", "Đà Nẵng"}
	genNoise    = []string{"Thanh toan don hang", "giao hàng tiêu chuẩn", "An error occurred", "status ok", "mode=test",
		"order_code", "san pham", "Long Bien", "an toan", "phi van chuyen"}
)

// genFold bỏ dấu tiếng Việt (bảng gọn riêng của bộ sinh, độc lập với gói redact).
func genFold(s string) string {
	r := strings.NewReplacer(
		"à", "a", "á", "a", "ạ", "a", "ả", "a", "ã", "a", "â", "a", "ầ", "a", "ấ", "a", "ậ", "a", "ẩ", "a", "ẫ", "a",
		"ă", "a", "ằ", "a", "ắ", "a", "ặ", "a", "ẳ", "a", "ẵ", "a", "è", "e", "é", "e", "ẹ", "e", "ẻ", "e", "ẽ", "e",
		"ê", "e", "ề", "e", "ế", "e", "ệ", "e", "ể", "e", "ễ", "e", "ì", "i", "í", "i", "ị", "i", "ỉ", "i", "ĩ", "i",
		"ò", "o", "ó", "o", "ọ", "o", "ỏ", "o", "õ", "o", "ô", "o", "ồ", "o", "ố", "o", "ộ", "o", "ổ", "o", "ỗ", "o",
		"ơ", "o", "ờ", "o", "ớ", "o", "ợ", "o", "ở", "o", "ỡ", "o", "ù", "u", "ú", "u", "ụ", "u", "ủ", "u", "ũ", "u",
		"ư", "u", "ừ", "u", "ứ", "u", "ự", "u", "ử", "u", "ữ", "u", "ỳ", "y", "ý", "y", "ỵ", "y", "ỷ", "y", "ỹ", "y",
		"đ", "d", "Đ", "D", "Ấ", "A", "Ả", "A", "Ô", "O", "Ư", "U", "Ă", "A", "Â", "A", "Ê", "E")
	return r.Replace(s)
}

func genPick(rng *rand.Rand, xs []string) string { return xs[rng.Intn(len(xs))] }

func genNewCustomer(rng *rand.Rand) genCustomer {
	name := genPick(rng, genSurnames) + " " + genPick(rng, genMiddles) + " " + genPick(rng, genGivens)
	var lines []string
	if rng.Intn(2) == 0 {
		lines = append(lines, fmt.Sprintf("%d %s", 1+rng.Intn(300), genPick(rng, genStreets)))
	}
	switch rng.Intn(3) {
	case 0: // dòng nhiều thành phần ngắn, không chữ số
		lines = append(lines, genPick(rng, genVillages)+", "+genPick(rng, genCommunes))
	case 1:
		lines = append(lines, genPick(rng, genCommunes))
	default:
		lines = append(lines, genPick(rng, genVillages)+" - "+genPick(rng, genCommunes))
	}
	lines = append(lines, genPick(rng, genDistrict)+", "+genPick(rng, genCities))
	return genCustomer{Name: name, Email: fmt.Sprintf("%s.%d+shop@example.com", strings.ToLower(genFold(strings.Fields(name)[2])), rng.Intn(1000)),
		Phone: fmt.Sprintf("09%08d", rng.Intn(1e8)), Address: strings.Join(lines, "\n")}
}

// genField là một trường (khoá, giá trị, loại bí mật; loại rỗng = không phải dữ liệu khách).
type genField struct{ Key, Value, Kind string }

// genKinds: loại bí mật. Loại "weak_standalone" là chữ không dấu của tên đứng riêng ở khoá lạ: theo thiết kế KHÔNG che.
func genFields(rng *rand.Rand, c genCustomer) []genField {
	words := strings.Fields(c.Name)
	lines := strings.Split(c.Address, "\n")
	var comps []string
	for _, l := range lines {
		for _, p := range strings.Split(strings.ReplaceAll(l, " - ", ","), ",") {
			comps = append(comps, strings.TrimSpace(p))
		}
	}
	all := []genField{
		{"name", c.Name, "name_full"},
		{"holder", strings.ToUpper(genFold(c.Name)), "name_folded"},
		{"first_name", words[1] + " " + words[2], "name_split"},
		{"last_name", words[0], "name_split"},
		{"lastName", strings.ToUpper(genFold(words[0])), "name_split_folded"},
		{"ho", strings.ToUpper(genFold(words[0])), "name_split_folded"},
		{"note", "giao cho " + words[0], "strong_standalone"},
		{"memo", strings.ToUpper(genFold(words[2])), "weak_standalone"},
		{"email", c.Email, "email"},
		{"mail", strings.ToUpper(c.Email), "email"},
		{"phone", c.Phone, "phone"},
		{"tel", "+84 " + c.Phone[1:4] + " " + c.Phone[4:7] + " " + c.Phone[7:], "phone"},
		{"msisdn", "(+84) " + c.Phone[1:], "phone"},
		{"mobile", c.Phone[:4] + "-" + c.Phone[4:7] + "-" + c.Phone[7:], "phone"},
		{"address", c.Address, "address_full"},
		{"addr", strings.Join(lines, ", "), "address_full"},
		{"ship_to", strings.Join(lines, " - "), "address_full"},
		genLine(lines[rng.Intn(len(lines))]),
		{"ward", comps[rng.Intn(len(comps))], "address_component_field"},
		{"street2", strings.ToUpper(genFold(comps[rng.Intn(len(comps))])), "address_component_field"},
		{"desc", genPick(rng, genNoise), ""},
		{"amount", fmt.Sprint(rng.Intn(1e7)), ""},
		{"order_code", fmt.Sprintf("ORD%08d", rng.Intn(1e8)), ""},
	}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	return all[:6+rng.Intn(len(all)-6)]
}

func genASCIIJSON(s string) string {
	b, _ := json.Marshal(s)
	var out strings.Builder
	for _, r := range string(b[1 : len(b)-1]) {
		if r < 0x80 {
			out.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			r -= 0x10000
			fmt.Fprintf(&out, `\u%04X\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			continue
		}
		fmt.Fprintf(&out, `\u%04x`, r)
	}
	return out.String()
}

func genJSONEsc(s string) string { b, _ := json.Marshal(s); return string(b[1 : len(b)-1]) }

// genEncoders: cách mã hoá giá trị trong thân và cách dựng thân. Mỗi phần tử: tên, mã hoá giá trị, dựng thân.
type genEncoder struct {
	name  string
	value func(string) string // giá trị → dạng nằm trong thân
	body  func(fs []genField, enc func(string) string) string
}

var genEncoders = []genEncoder{
	{"json", genJSONEsc, genJSONBody},
	{"json_u", genASCIIJSON, genJSONBody},
	{"form", url.QueryEscape, genFormBody},
	{"form_path", url.PathEscape, genFormBody},
	{"xml", func(s string) string { return s }, genXMLBody},
	{"text", func(s string) string { return s }, genTextBody},
	// JSON lồng trong chuỗi JSON: giá trị escape hai lần.
	{"json_in_json", func(s string) string { return genJSONEsc(genJSONEsc(s)) }, func(fs []genField, _ func(string) string) string {
		inner := genJSONBody(fs, genJSONEsc)
		return `{"payload":"` + genJSONEsc(inner) + `"}`
	}},
	// JSON lồng trong form: giá trị escape JSON rồi QueryEscape.
	{"json_in_form", func(s string) string { return url.QueryEscape(genJSONEsc(s)) }, func(fs []genField, _ func(string) string) string {
		return "data=" + url.QueryEscape(genJSONBody(fs, genJSONEsc)) + "&sig=abc"
	}},
}

func genJSONBody(fs []genField, enc func(string) string) string {
	var b strings.Builder
	b.WriteString("{")
	for i, f := range fs {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%s":"%s"`, f.Key, enc(f.Value))
	}
	b.WriteString("}")
	return b.String()
}

func genFormBody(fs []genField, enc func(string) string) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, f.Key+"="+enc(f.Value))
	}
	return strings.Join(parts, "&")
}

func genXMLBody(fs []genField, enc func(string) string) string {
	var b strings.Builder
	b.WriteString("<req>")
	for _, f := range fs {
		fmt.Fprintf(&b, "<%s>%s</%s>", f.Key, enc(f.Value), f.Key)
	}
	b.WriteString("</req>")
	return b.String()
}

func genTextBody(fs []genField, enc func(string) string) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, enc(f.Value))
	}
	return "Thong tin: " + strings.Join(parts, " | ") + "."
}

// genBodies sinh n thân với hạt giống seed.
func genBodies(seed int64, n int) []genBody {
	rng := rand.New(rand.NewSource(seed))
	out := make([]genBody, 0, n)
	for i := 0; i < n; i++ {
		c := genNewCustomer(rng)
		fs := genFields(rng, c)
		e := genEncoders[rng.Intn(len(genEncoders))]
		gb := genBody{Customer: c, Body: e.body(fs, e.value)}
		for _, f := range fs {
			if f.Kind != "" {
				gb.Secrets = append(gb.Secrets, genSecret{Kind: e.name + "/" + f.Kind, Encoded: e.value(f.Value)})
			}
		}
		out = append(out, gb)
	}
	return out
}

// genLine: dòng địa chỉ ≥ 12 ký tự phải che ở mọi chỗ; dòng ngắn hơn chỉ phải che ở trường gợi ý địa chỉ.
func genLine(l string) genField {
	if len([]rune(l)) >= 12 {
		return genField{"address_line1", l, "address_line"}
	}
	return genField{"address_line1", l, "address_line_short"}
}
