package diff

import (
	"bytes"
	"fmt"
	"strconv"
	"testing"
)

// body builds a JSON object with n order lines, about 70 bytes each.
func body(n int, tweak func(i int) (sku string, qty int)) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":1,"status":"paid","total":123.45,"customer":{"id":7,"name":"Asha","tier":"gold"},"items":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		sku, qty := "SKU-"+strconv.Itoa(i), 1+i%5
		if tweak != nil {
			sku, qty = tweak(i)
		}
		fmt.Fprintf(&b, `{"sku":%q,"qty":%d,"price":%d.99,"tags":["a","b"]}`, sku, qty, i%90)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// BenchmarkCompare measures one response pair. "same" is the common case (no
// change); "diff" has a changed value on every tenth line.
func BenchmarkCompare(b *testing.B) {
	for _, size := range []struct {
		name  string
		lines int
	}{{"200B", 1}, {"8KB", 100}, {"64KB", 800}} {
		base := body(size.lines, nil)
		changed := body(size.lines, func(i int) (string, int) {
			if i%10 == 0 {
				return "SKU-x" + strconv.Itoa(i), 9
			}
			return "SKU-" + strconv.Itoa(i), 1 + i%5
		})
		for _, c := range []struct {
			name string
			cand []byte
		}{{"same", base}, {"diff", changed}} {
			b.Run(size.name+"/"+c.name, func(b *testing.B) {
				bs := Response{Status: 200, Body: base, LatencyMS: 5}
				cs := Response{Status: 200, Body: c.cand, LatencyMS: 5}
				cfg := Config{FloatTolerance: 1e-9, LatencyFactor: 1.5, LatencyMinMS: 50}
				b.SetBytes(int64(len(base)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_ = Compare(bs, cs, cfg)
				}
			})
		}
	}
}
