package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
	"golang.org/x/net/html"
)

const maxUpload = 10 << 20
const maxDocumentText = 180000 // Unicode characters; at most 200 overlapping chunks.

func extractDocument(name string, data []byte) (text string, err error) {
	// Malformed third-party document structures must not crash the server.
	defer func() {
		if recover() != nil {
			text = ""
			err = fmt.Errorf("文档损坏或无法解析")
		}
	}()
	var out strings.Builder
	appendText := func(v string) error {
		if out.Len()+len(v) > maxDocumentText*4 {
			return fmt.Errorf("文档文字过多，请拆分为较小的文件")
		}
		out.WriteString(v)
		return nil
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".csv", ".json":
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return "", fmt.Errorf("请使用 UTF-8 编码的文本文件")
		}
		if filepath.Ext(strings.ToLower(name)) == ".json" && !json.Valid(data) {
			return "", fmt.Errorf("JSON 文件格式不正确")
		}
		if err = appendText(string(data)); err != nil {
			return "", err
		}
	case ".html", ".htm":
		doc, e := html.Parse(bytes.NewReader(data))
		if e != nil {
			return "", fmt.Errorf("HTML 文件无法解析")
		}
		var walk func(*html.Node) error
		walk = func(n *html.Node) error {
			if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style" || n.Data == "noscript") {
				return nil
			}
			if n.Type == html.TextNode {
				if e := appendText(n.Data + " "); e != nil {
					return e
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if e := walk(c); e != nil {
					return e
				}
			}
			return nil
		}
		if err = walk(doc); err != nil {
			return "", err
		}
	case ".docx":
		z, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if e != nil {
			return "", fmt.Errorf("DOCX 文件无法解析")
		}
		found := false
		for _, file := range z.File {
			if file.Name != "word/document.xml" {
				continue
			}
			found = true
			if file.UncompressedSize64 > 20<<20 {
				return "", fmt.Errorf("DOCX 解压内容过大")
			}
			r, e := file.Open()
			if e != nil {
				return "", fmt.Errorf("DOCX 文件损坏")
			}
			d := xml.NewDecoder(io.LimitReader(r, 20<<20))
			inText := false
			for {
				tok, e := d.Token()
				if e == io.EOF {
					break
				}
				if e != nil {
					r.Close()
					return "", fmt.Errorf("DOCX 文本无法解析")
				}
				switch v := tok.(type) {
				case xml.StartElement:
					if v.Name.Local == "t" {
						inText = true
					}
					if v.Name.Local == "tab" || v.Name.Local == "br" {
						err = appendText("\n")
					}
				case xml.EndElement:
					if v.Name.Local == "t" {
						inText = false
					}
					if v.Name.Local == "p" {
						err = appendText("\n")
					}
				case xml.CharData:
					if inText {
						err = appendText(string(v))
					}
				}
				if err != nil {
					r.Close()
					return "", err
				}
			}
			r.Close()
		}
		if !found {
			return "", fmt.Errorf("DOCX 缺少正文")
		}
	case ".xlsx":
		f, e := excelize.OpenReader(bytes.NewReader(data), excelize.Options{UnzipSizeLimit: 20 << 20, UnzipXMLSizeLimit: 20 << 20})
		if e != nil {
			return "", fmt.Errorf("XLSX 文件无法解析或解压内容过大")
		}
		defer f.Close()
		for _, sheet := range f.GetSheetList() {
			if err = appendText("\n" + sheet + "\n"); err != nil {
				return "", err
			}
			rows, e := f.Rows(sheet)
			if e != nil {
				return "", fmt.Errorf("工作表无法读取")
			}
			for rows.Next() {
				cells, e := rows.Columns()
				if e != nil {
					rows.Close()
					return "", fmt.Errorf("单元格无法读取")
				}
				if err = appendText(strings.Join(cells, "\t") + "\n"); err != nil {
					rows.Close()
					return "", err
				}
			}
			e = rows.Error()
			rows.Close()
			if e != nil {
				return "", fmt.Errorf("工作表不完整")
			}
		}
	case ".pdf":
		f, e := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
		if e != nil {
			return "", fmt.Errorf("PDF 文件无法解析，可能损坏或已加密")
		}
		if f.NumPage() > 200 {
			return "", fmt.Errorf("PDF 超过 200 页，请拆分上传")
		}
		for i := 1; i <= f.NumPage(); i++ {
			page := f.Page(i)
			if page.V.IsNull() {
				continue
			}
			v, e := page.GetPlainText(nil)
			if e != nil {
				return "", fmt.Errorf("PDF 第 %d 页无法读取", i)
			}
			if err = appendText(v + "\n"); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("支持 PDF、DOCX、TXT、Markdown、HTML、CSV、JSON、XLSX；旧 XLS 请先另存为 XLSX")
	}
	text = strings.TrimSpace(out.String())
	if text == "" {
		return "", fmt.Errorf("未提取到文字；扫描 PDF 请先进行 OCR")
	}
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxDocumentText {
		return "", fmt.Errorf("文档文字过多或编码无效，请拆分或转换后上传")
	}
	return text, nil
}

func documentChunks(text string) []string {
	runes := []rune(text)
	result := []string{}
	for start := 0; start < len(runes); start += 900 {
		end := min(start+1000, len(runes))
		if v := strings.TrimSpace(string(runes[start:end])); v != "" {
			result = append(result, v)
		}
		if end == len(runes) {
			break
		}
	}
	return result
}
