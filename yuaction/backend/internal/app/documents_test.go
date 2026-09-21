package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"github.com/xuri/excelize/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocumentExtractionAndUnicodeChunks(t *testing.T) {
	for _, name := range []string{"notes.txt", "notes.md", "notes.csv"} {
		text, err := extractDocument(name, []byte("你好，课堂"))
		if err != nil || text != "你好，课堂" {
			t.Fatalf("%s: %q %v", name, text, err)
		}
	}
	for _, v := range []struct {
		name string
		data []byte
	}{{"bad.txt", []byte{0xff}}, {"bad.json", []byte(`{`)}, {"bad.docx", []byte("not zip")}, {"old.xls", []byte("binary")}} {
		if _, err := extractDocument(v.name, v.data); err == nil {
			t.Fatalf("invalid %s accepted", v.name)
		}
	}
	text, err := extractDocument("notes.html", []byte(`<script>SECRET</script><p>Visible text</p>`))
	if err != nil || strings.Contains(text, "SECRET") || !strings.Contains(text, "Visible text") {
		t.Fatalf("html: %q %v", text, err)
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, _ := z.Create("word/document.xml")
	_, _ = w.Write([]byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>讲义内容</w:t></w:r></w:p></w:body></w:document>`))
	_ = z.Close()
	text, err = extractDocument("notes.docx", b.Bytes())
	if err != nil || !strings.Contains(text, "讲义内容") {
		t.Fatalf("docx: %q %v", text, err)
	}
	sheet := excelize.NewFile()
	defer sheet.Close()
	_ = sheet.SetCellValue("Sheet1", "A1", "课堂数据")
	data, err := sheet.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	text, err = extractDocument("notes.xlsx", data.Bytes())
	if err != nil || !strings.Contains(text, "课堂数据") {
		t.Fatalf("xlsx: %q %v", text, err)
	}
	chunks := documentChunks(strings.Repeat("课堂知识", 800))
	if len(chunks) < 3 {
		t.Fatal("large text not chunked")
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk) || utf8.RuneCountInString(chunk) > 1000 {
			t.Fatal("invalid unicode chunk")
		}
	}
}
func TestEmbeddingRejectsDuplicateIndexes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{1, 0}}, map[string]any{"index": 0, "embedding": []float64{1, 0}}}})
	}))
	defer upstream.Close()
	cfg := AIConfig{EmbeddingKey: "fake", EmbeddingModel: "test", EmbeddingURL: upstream.URL}
	if _, err := cfg.embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("duplicate embedding index accepted")
	}
}
