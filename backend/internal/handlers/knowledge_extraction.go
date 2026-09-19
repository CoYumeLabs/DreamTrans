package handlers

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/dreamtrans/backend/internal/deployment"
	"image"
	"regexp"
	"sort"
	// Register JPEG decoding for knowledge image validation.
	_ "image/jpeg"
	// Register PNG decoding for knowledge image validation.
	_ "image/png"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

const (
	defaultKnowledgeExtractedBytes          = int64(10 << 20)
	defaultKnowledgeOfficeUncompressedBytes = int64(100 << 20)
	defaultKnowledgeImagePixels             = int64(40_000_000)
	defaultKnowledgePDFPages                = 100
	defaultKnowledgeExtractWorkers          = 2
	maxKnowledgeExtractWorkers              = 32
	knowledgeTaskTimeout                    = 20 * time.Minute
	knowledgeCommandTimeout                 = 2 * time.Minute
	knowledgeExtractionLease                = 3 * time.Minute
	knowledgeExtractionLeaseRenewal         = time.Minute
	knowledgeCommandErrorBytes              = int64(64 << 10)
	knowledgeRasterBytes                    = int64(64 << 20)
	knowledgePDFRenderMaxDimension          = 6000
	knowledgeOfficeMaxEntries               = 10_000
	knowledgeOfficeMaxCompressionRatio      = uint64(1_000)
	knowledgeSpreadsheetMaxSharedStrings    = 250_000
	knowledgeSpreadsheetMaxCells            = 1_000_000
)

var (
	errKnowledgeExtractedTextTooLarge = errors.New("extracted text exceeds configured limit")
	errKnowledgeOfficeTooLarge        = errors.New("office archive exceeds decompressed size limit")
	errKnowledgeImageTooLarge         = errors.New("image exceeds pixel limit")
	errKnowledgeCommandOutputTooLarge = errors.New("extractor output exceeds configured limit")
	errKnowledgeSpreadsheetTooComplex = errors.New("spreadsheet exceeds item or cell limit")
)

var allowedKnowledgeOCRLanguages = map[string]struct{}{
	"eng":     {},
	"chi_sim": {},
	"jpn":     {},
	"kor":     {},
}

type knowledgeExtractionLimits struct {
	maxExtractedBytes          int64
	maxOfficeUncompressedBytes int64
	maxImagePixels             int64
	maxPDFPages                int
}

func currentKnowledgeExtractionLimits() knowledgeExtractionLimits {
	return knowledgeExtractionLimits{
		maxExtractedBytes: envMegabytes(
			"KNOWLEDGE_MAX_EXTRACTED_MB",
			defaultKnowledgeExtractedBytes,
			1,
			500,
		),
		maxOfficeUncompressedBytes: envMegabytes(
			"KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB",
			defaultKnowledgeOfficeUncompressedBytes,
			1,
			2_000,
		),
		maxImagePixels: envInteger(
			"KNOWLEDGE_MAX_IMAGE_MEGAPIXELS",
			defaultKnowledgeImagePixels/1_000_000,
			1,
			500,
		) * 1_000_000,
		maxPDFPages: int(envInteger(
			"KNOWLEDGE_MAX_PDF_PAGES",
			defaultKnowledgePDFPages,
			1,
			2_000,
		)),
	}
}

func envMegabytes(name string, fallback int64, minimum, maximum int64) int64 {
	value := envInteger(name, fallback>>20, minimum, maximum)
	return value << 20
}

func envInteger(name string, fallback, minimum, maximum int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return fallback
	}
	return value
}

func knowledgeExtractWorkerCount() int {
	return int(envInteger(
		"KNOWLEDGE_EXTRACT_WORKERS",
		defaultKnowledgeExtractWorkers,
		1,
		maxKnowledgeExtractWorkers,
	))
}

func parseKnowledgeOCRLanguages(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{"eng", "chi_sim"}, nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if _, ok := allowedKnowledgeOCRLanguages[value]; !ok {
			return nil, fmt.Errorf(
				"unsupported OCR language %q (allowed: eng, chi_sim, jpn, kor)",
				value,
			)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, errors.New("ocr_language must not be empty")
	}
	return result, nil
}

func defaultKnowledgeOCRLanguages(sourceLanguage string) []string {
	normalized := strings.ToLower(strings.TrimSpace(sourceLanguage))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch {
	case normalized == "ja", normalized == "jpn",
		strings.HasPrefix(normalized, "ja-"):
		return []string{"jpn"}
	case normalized == "ko", normalized == "kor",
		strings.HasPrefix(normalized, "ko-"):
		return []string{"kor"}
	case normalized == "zh", normalized == "cmn", normalized == "chi",
		normalized == "zho", strings.HasPrefix(normalized, "zh-"):
		return []string{"chi_sim"}
	case normalized == "en", normalized == "eng",
		strings.HasPrefix(normalized, "en-"):
		return []string{"eng"}
	default:
		return []string{"eng", "chi_sim"}
	}
}

func normalizeStoredOCRLanguages(values []string) []string {
	normalized, err := parseKnowledgeOCRLanguages(values)
	if err != nil {
		return []string{"eng", "chi_sim"}
	}
	return normalized
}

func tesseractLanguageArgument(values []string) string {
	return strings.Join(normalizeStoredOCRLanguages(values), "+")
}

func validateKnowledgeUpload(
	path, extension, declaredMediaType string,
) (string, error) {
	file, err := os.Open(path) //nolint:gosec // path is generated under the configured data root.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 512)
	read, readErr := io.ReadFull(file, header)
	if readErr != nil && !errors.Is(readErr, io.EOF) &&
		!errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "", readErr
	}
	detected, _, err := mime.ParseMediaType(http.DetectContentType(header[:read]))
	if err != nil {
		return "", errors.New("invalid detected media type")
	}
	detected = strings.ToLower(detected)
	declared := ""
	if strings.TrimSpace(declaredMediaType) != "" {
		declared, _, err = mime.ParseMediaType(declaredMediaType)
		if err != nil {
			return "", errors.New("invalid upload media type")
		}
		declared = strings.ToLower(declared)
	}
	canonical, magicAllowed, declaredAllowed := knowledgeMediaTypes(extension)
	if canonical == "" || !mediaTypeAllowed(detected, magicAllowed) {
		return "", fmt.Errorf(
			"file contents do not match the %s extension",
			extension,
		)
	}
	if declared != "" && declared != "application/octet-stream" &&
		!mediaTypeAllowed(declared, declaredAllowed) {
		return "", fmt.Errorf(
			"declared media type %s does not match the %s extension",
			declared,
			extension,
		)
	}
	limits := currentKnowledgeExtractionLimits()
	switch extension {
	case ".docx", ".xlsx", ".pptx":
		if err := validateOfficeContainer(path, extension, limits); err != nil {
			return "", err
		}
	case ".png", ".jpg", ".jpeg", ".webp":
		if err := validateKnowledgeImage(path, extension, limits.maxImagePixels); err != nil {
			return "", err
		}
	}
	return canonical, nil
}

func knowledgeMediaTypes(extension string) (string, []string, []string) {
	switch extension {
	case ".txt", ".md":
		return "text/plain", []string{"text/plain"}, []string{"text/plain", "text/markdown"}
	case ".csv":
		return "text/csv", []string{"text/plain"}, []string{
			"text/plain", "text/csv", "application/csv", "application/vnd.ms-excel",
		}
	case ".tsv":
		return "text/tab-separated-values", []string{"text/plain"}, []string{
			"text/plain", "text/tab-separated-values",
		}
	case ".json":
		return "application/json", []string{"text/plain", "application/json"}, []string{
			"text/plain", "application/json",
		}
	case ".pdf":
		return "application/pdf", []string{"application/pdf"}, []string{"application/pdf"}
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			[]string{"application/zip"},
			[]string{
				"application/zip",
				"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			}
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			[]string{"application/zip"},
			[]string{
				"application/zip",
				"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			}
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation",
			[]string{"application/zip"},
			[]string{
				"application/zip",
				"application/vnd.openxmlformats-officedocument.presentationml.presentation",
			}
	case ".png":
		return "image/png", []string{"image/png"}, []string{"image/png"}
	case ".jpg", ".jpeg":
		return "image/jpeg", []string{"image/jpeg"}, []string{"image/jpeg"}
	case ".webp":
		return "image/webp", []string{"image/webp"}, []string{"image/webp"}
	default:
		return "", nil, nil
	}
}

func mediaTypeAllowed(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func isDuplicateKnowledgeSourceError(err error) bool {
	var sqlState interface{ SQLState() string }
	return errors.As(err, &sqlState) && sqlState.SQLState() == "23505"
}

func extractKnowledgeText(
	ctx context.Context,
	path string,
	extension string,
	ocrLanguages []string,
) (string, error) {
	limits := currentKnowledgeExtractionLimits()
	var (
		text string
		err  error
	)
	switch extension {
	case ".txt", ".md", ".json":
		text, err = readBoundedTextFile(path, limits.maxExtractedBytes)
	case ".csv", ".tsv":
		text, err = extractDelimited(path, extension == ".tsv", limits.maxExtractedBytes)
	case ".docx":
		text, err = extractDOCX(path, limits)
	case ".xlsx":
		text, err = extractXLSX(path, limits)
	case ".pptx":
		text, err = extractPPTX(path, limits)
	case ".pdf":
		text, err = extractPDFText(
			ctx,
			path,
			ocrLanguages,
			limits,
			runBoundedKnowledgeCommand,
		)
	case ".png", ".jpg", ".jpeg", ".webp":
		if err = validateKnowledgeImage(path, extension, limits.maxImagePixels); err == nil {
			var output []byte
			output, err = runBoundedKnowledgeCommand(
				ctx,
				limits.maxExtractedBytes,
				"tesseract",
				path,
				"stdout",
				"-l",
				tesseractLanguageArgument(ocrLanguages),
			)
			text = string(output)
		}
	default:
		err = errors.New("unsupported file type")
	}
	if err != nil {
		return "", err
	}
	if int64(len(text)) > limits.maxExtractedBytes {
		return "", errKnowledgeExtractedTextTooLarge
	}
	return text, nil
}

func readBoundedTextFile(path string, limit int64) (string, error) {
	file, err := os.Open(path) //nolint:gosec // path is generated under the configured data root.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", errKnowledgeExtractedTextTooLarge
	}
	return string(data), nil
}

type boundedTextBuilder struct {
	builder strings.Builder
	limit   int64
}

func (b *boundedTextBuilder) WriteString(value string) error {
	if int64(b.builder.Len())+int64(len(value)) > b.limit {
		return errKnowledgeExtractedTextTooLarge
	}
	b.builder.WriteString(value)
	return nil
}

func (b *boundedTextBuilder) WriteByte(value byte) error {
	if int64(b.builder.Len())+1 > b.limit {
		return errKnowledgeExtractedTextTooLarge
	}
	return b.builder.WriteByte(value)
}

func (b *boundedTextBuilder) String() string {
	return b.builder.String()
}

func extractDelimited(path string, tabSeparated bool, maxTextBytes int64) (string, error) {
	file, err := os.Open(path) //nolint:gosec // generated knowledge path.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	reader := csv.NewReader(bufio.NewReader(file))
	reader.FieldsPerRecord = -1
	if tabSeparated {
		reader.Comma = '\t'
	}
	var builder boundedTextBuilder
	builder.limit = maxTextBytes
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
		if err := builder.WriteString(strings.Join(record, " | ")); err != nil {
			return "", err
		}
		if err := builder.WriteByte('\n'); err != nil {
			return "", err
		}
	}
	return builder.String(), nil
}

type boundedOfficeArchive struct {
	archive *zip.ReadCloser
	File    []*zip.File
	limit   int64
	used    int64
}

func (a *boundedOfficeArchive) Close() error {
	return a.archive.Close()
}

func (a *boundedOfficeArchive) Open(file *zip.File) (io.ReadCloser, error) {
	stream, err := file.Open()
	if err != nil {
		return nil, err
	}
	return &boundedOfficeMemberReader{
		ReadCloser: stream,
		archive:    a,
	}, nil
}

type boundedOfficeMemberReader struct {
	io.ReadCloser
	archive *boundedOfficeArchive
}

func (r *boundedOfficeMemberReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	remaining := r.archive.limit - r.archive.used
	if remaining <= 0 {
		return 0, errKnowledgeOfficeTooLarge
	}
	readLimit := int64(len(buffer))
	if readLimit > remaining+1 {
		readLimit = remaining + 1
	}
	n, err := r.ReadCloser.Read(buffer[:int(readLimit)])
	if int64(n) > remaining {
		r.archive.used += int64(n)
		return 0, errKnowledgeOfficeTooLarge
	}
	r.archive.used += int64(n)
	return n, err
}

func openBoundedOfficeArchive(
	path string, maxUncompressedBytes int64,
) (*boundedOfficeArchive, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	if len(reader.File) > knowledgeOfficeMaxEntries {
		_ = reader.Close()
		return nil, errKnowledgeOfficeTooLarge
	}
	var total int64
	for _, file := range reader.File {
		size := file.UncompressedSize64
		if size > math.MaxInt64 {
			_ = reader.Close()
			return nil, errKnowledgeOfficeTooLarge
		}
		uncompressedSize := int64(size)
		if uncompressedSize > maxUncompressedBytes ||
			total > maxUncompressedBytes-uncompressedSize {
			_ = reader.Close()
			return nil, errKnowledgeOfficeTooLarge
		}
		if size > 0 {
			compressed := file.CompressedSize64
			if compressed == 0 ||
				size/compressed > knowledgeOfficeMaxCompressionRatio {
				_ = reader.Close()
				return nil, errKnowledgeOfficeTooLarge
			}
		}
		total += uncompressedSize
	}
	return &boundedOfficeArchive{
		archive: reader,
		File:    reader.File,
		limit:   maxUncompressedBytes,
	}, nil
}

func validateOfficeContainer(
	path, extension string, limits knowledgeExtractionLimits,
) error {
	reader, err := openBoundedOfficeArchive(path, limits.maxOfficeUncompressedBytes)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	required := map[string]bool{"[Content_Types].xml": false}
	switch extension {
	case ".docx":
		required["word/document.xml"] = false
	case ".xlsx":
		required["xl/workbook.xml"] = false
	case ".pptx":
		required["ppt/presentation.xml"] = false
	default:
		return errors.New("unsupported office file type")
	}
	for _, file := range reader.File {
		if _, ok := required[file.Name]; ok {
			required[file.Name] = true
		}
	}
	for member, found := range required {
		if !found {
			return fmt.Errorf("%s was not found in office document", member)
		}
	}
	return nil
}

func extractDOCX(path string, limits knowledgeExtractionLimits) (string, error) {
	const member = "word/document.xml"
	reader, err := openBoundedOfficeArchive(path, limits.maxOfficeUncompressedBytes)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()
	for _, file := range reader.File {
		if file.Name != member {
			continue
		}
		stream, err := reader.Open(file)
		if err != nil {
			return "", err
		}
		text, parseErr := extractXMLTextBounded(stream, limits.maxExtractedBytes)
		closeErr := stream.Close()
		if parseErr != nil {
			return "", parseErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return text, nil
	}
	return "", fmt.Errorf("%s was not found in document", member)
}

var pptxSlideName = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)

// extractPPTX reads every slide (and its notes) in deck order. Each slide is
// labeled so a lecture deck keeps its structure in the extracted text.
func extractPPTX(path string, limits knowledgeExtractionLimits) (string, error) {
	reader, err := openBoundedOfficeArchive(path, limits.maxOfficeUncompressedBytes)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()
	type slideFile struct {
		number int
		file   *zip.File
	}
	slides := make([]slideFile, 0)
	notes := make(map[int]*zip.File)
	for _, file := range reader.File {
		if match := pptxSlideName.FindStringSubmatch(file.Name); match != nil {
			number, _ := strconv.Atoi(match[1])
			slides = append(slides, slideFile{number: number, file: file})
			continue
		}
		if strings.HasPrefix(file.Name, "ppt/notesSlides/notesSlide") &&
			strings.HasSuffix(file.Name, ".xml") {
			numberText := strings.TrimSuffix(
				strings.TrimPrefix(file.Name, "ppt/notesSlides/notesSlide"), ".xml",
			)
			if number, convErr := strconv.Atoi(numberText); convErr == nil {
				notes[number] = file
			}
		}
	}
	if len(slides) == 0 {
		return "", errors.New("presentation has no slides")
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].number < slides[j].number })
	builder := &boundedTextBuilder{limit: limits.maxExtractedBytes}
	readMember := func(file *zip.File) (string, error) {
		stream, openErr := reader.Open(file)
		if openErr != nil {
			return "", openErr
		}
		text, parseErr := extractXMLTextBounded(stream, limits.maxExtractedBytes)
		closeErr := stream.Close()
		if parseErr != nil {
			return "", parseErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return strings.TrimSpace(text), nil
	}
	for index, slide := range slides {
		body, readErr := readMember(slide.file)
		if readErr != nil {
			return "", readErr
		}
		if index > 0 {
			if err := builder.WriteString("\n\n"); err != nil {
				return "", err
			}
		}
		if err := builder.WriteString(fmt.Sprintf("[Slide %d]\n%s", slide.number, body)); err != nil {
			return "", err
		}
		if noteFile, ok := notes[slide.number]; ok {
			noteText, noteErr := readMember(noteFile)
			if noteErr != nil {
				return "", noteErr
			}
			if noteText != "" {
				if err := builder.WriteString("\n[Notes]\n" + noteText); err != nil {
					return "", err
				}
			}
		}
	}
	return builder.String(), nil
}

func extractXLSX(path string, limits knowledgeExtractionLimits) (string, error) {
	reader, err := openBoundedOfficeArchive(path, limits.maxOfficeUncompressedBytes)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()
	sharedStrings := make([]string, 0)
	for _, file := range reader.File {
		if file.Name != "xl/sharedStrings.xml" {
			continue
		}
		stream, openErr := reader.Open(file)
		if openErr != nil {
			return "", openErr
		}
		sharedStrings, err = extractSharedStringsBounded(
			stream,
			limits.maxExtractedBytes,
		)
		_ = stream.Close()
		if err != nil {
			return "", err
		}
		break
	}
	var builder boundedTextBuilder
	builder.limit = limits.maxExtractedBytes
	remainingCells := knowledgeSpreadsheetMaxCells
	for _, file := range reader.File {
		if !strings.HasPrefix(file.Name, "xl/worksheets/") ||
			!strings.HasSuffix(file.Name, ".xml") {
			continue
		}
		stream, openErr := reader.Open(file)
		if openErr != nil {
			return "", openErr
		}
		remaining := limits.maxExtractedBytes - int64(builder.builder.Len())
		text, cellsRead, parseErr := extractWorksheetTextLimited(
			stream,
			sharedStrings,
			remaining,
			remainingCells,
		)
		_ = stream.Close()
		if parseErr != nil {
			return "", parseErr
		}
		remainingCells -= cellsRead
		if err := builder.WriteString("[" + filepath.Base(file.Name) + "]\n"); err != nil {
			return "", err
		}
		if err := builder.WriteString(text); err != nil {
			return "", err
		}
		if err := builder.WriteByte('\n'); err != nil {
			return "", err
		}
	}
	return builder.String(), nil
}

func extractSharedStringsBounded(reader io.Reader, maxTextBytes int64) ([]string, error) {
	return extractSharedStringsLimited(
		reader,
		maxTextBytes,
		knowledgeSpreadsheetMaxSharedStrings,
	)
}

func extractSharedStringsLimited(
	reader io.Reader,
	maxTextBytes int64,
	maxItems int,
) ([]string, error) {
	decoder := xml.NewDecoder(reader)
	values := make([]string, 0)
	var current boundedTextBuilder
	current.limit = maxTextBytes
	var totalBytes int64
	itemCount := 0
	inItem := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "si" {
				itemCount++
				if itemCount > maxItems {
					return nil, errKnowledgeSpreadsheetTooComplex
				}
				current = boundedTextBuilder{limit: maxTextBytes - totalBytes}
				inItem = true
			}
			if value.Name.Local == "t" && inItem {
				var text string
				if err := decoder.DecodeElement(&text, &value); err != nil {
					return nil, err
				}
				if err := current.WriteString(text); err != nil {
					return nil, err
				}
			}
		case xml.EndElement:
			if value.Name.Local == "si" && inItem {
				item := strings.TrimSpace(current.String())
				totalBytes += int64(len(item))
				if totalBytes > maxTextBytes {
					return nil, errKnowledgeExtractedTextTooLarge
				}
				values = append(values, item)
				inItem = false
			}
		}
	}
	return values, nil
}

func extractWorksheetText(reader io.Reader, sharedStrings []string) (string, error) {
	return extractWorksheetTextBounded(
		reader,
		sharedStrings,
		defaultKnowledgeExtractedBytes,
	)
}

func extractWorksheetTextBounded(
	reader io.Reader, sharedStrings []string, maxTextBytes int64,
) (string, error) {
	text, _, err := extractWorksheetTextLimited(
		reader,
		sharedStrings,
		maxTextBytes,
		knowledgeSpreadsheetMaxCells,
	)
	return text, err
}

func extractWorksheetTextLimited(
	reader io.Reader,
	sharedStrings []string,
	maxTextBytes int64,
	maxCells int,
) (string, int, error) {
	decoder := xml.NewDecoder(reader)
	var builder boundedTextBuilder
	builder.limit = maxTextBytes
	cellType := ""
	cellCount := 0
	wroteCell := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", cellCount, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "c":
				cellCount++
				if cellCount > maxCells {
					return "", cellCount, errKnowledgeSpreadsheetTooComplex
				}
				cellType = ""
				for _, attribute := range value.Attr {
					if attribute.Name.Local == "t" {
						cellType = attribute.Value
					}
				}
			case "v", "t":
				var text string
				if err := decoder.DecodeElement(&text, &value); err != nil {
					return "", cellCount, err
				}
				if value.Name.Local == "v" && cellType == "s" {
					index, parseErr := strconv.Atoi(strings.TrimSpace(text))
					if parseErr == nil && index >= 0 && index < len(sharedStrings) {
						text = sharedStrings[index]
					}
				}
				text = strings.TrimSpace(text)
				if text != "" {
					if wroteCell {
						if err := builder.WriteString(" | "); err != nil {
							return "", cellCount, err
						}
					}
					if err := builder.WriteString(text); err != nil {
						return "", cellCount, err
					}
					wroteCell = true
				}
			}
		case xml.EndElement:
			if value.Name.Local == "row" {
				if err := builder.WriteByte('\n'); err != nil {
					return "", cellCount, err
				}
				wroteCell = false
			}
		}
	}
	return builder.String(), cellCount, nil
}

func extractXMLTextBounded(reader io.Reader, maxTextBytes int64) (string, error) {
	decoder := xml.NewDecoder(reader)
	var builder boundedTextBuilder
	builder.limit = maxTextBytes
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "t", "v":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return "", err
			}
			value = strings.TrimSpace(value)
			if value != "" {
				if err := builder.WriteString(value); err != nil {
					return "", err
				}
				if err := builder.WriteByte(' '); err != nil {
					return "", err
				}
			}
		}
	}
	return builder.String(), nil
}

func validateKnowledgeImage(path, extension string, maxPixels int64) error {
	file, err := os.Open(path) //nolint:gosec // path is generated under the configured data root.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	var width, height int64
	if extension == ".webp" {
		header := make([]byte, 30)
		if _, err := io.ReadFull(file, header); err != nil {
			return errors.New("invalid WebP image")
		}
		width, height, err = webPDimensions(header)
		if err != nil {
			return err
		}
	} else {
		config, _, decodeErr := image.DecodeConfig(file)
		if decodeErr != nil {
			return errors.New("invalid image")
		}
		width, height = int64(config.Width), int64(config.Height)
	}
	if width <= 0 || height <= 0 || width > maxPixels ||
		height > maxPixels || width > maxPixels/height {
		return errKnowledgeImageTooLarge
	}
	return nil
}

func webPDimensions(header []byte) (int64, int64, error) {
	if len(header) < 30 || string(header[:4]) != "RIFF" ||
		string(header[8:12]) != "WEBP" {
		return 0, 0, errors.New("invalid WebP image")
	}
	switch string(header[12:16]) {
	case "VP8X":
		width := int64(header[24]) | int64(header[25])<<8 | int64(header[26])<<16
		height := int64(header[27]) | int64(header[28])<<8 | int64(header[29])<<16
		return width + 1, height + 1, nil
	case "VP8 ":
		if header[23] != 0x9d || header[24] != 0x01 || header[25] != 0x2a {
			return 0, 0, errors.New("invalid lossy WebP image")
		}
		width := int64(binary.LittleEndian.Uint16(header[26:28]) & 0x3fff)
		height := int64(binary.LittleEndian.Uint16(header[28:30]) & 0x3fff)
		return width, height, nil
	case "VP8L":
		if header[20] != 0x2f {
			return 0, 0, errors.New("invalid lossless WebP image")
		}
		b1, b2, b3, b4 := header[21], header[22], header[23], header[24]
		width := 1 + int64(b1) + int64(b2&0x3f)<<8
		height := 1 + int64(b2>>6) + int64(b3)<<2 + int64(b4&0x0f)<<10
		return width, height, nil
	default:
		return 0, 0, errors.New("unsupported WebP encoding")
	}
}

type knowledgeCommandRunner func(
	context.Context, int64, string, ...string,
) ([]byte, error)

func extractPDFText(
	ctx context.Context,
	path string,
	ocrLanguages []string,
	limits knowledgeExtractionLimits,
	run knowledgeCommandRunner,
) (string, error) {
	output, err := run(
		ctx,
		limits.maxExtractedBytes,
		"pdftotext",
		"-layout",
		path,
		"-",
	)
	if err != nil {
		return "", err
	}
	if !pdfNeedsOCR(string(output)) {
		return string(output), nil
	}
	info, err := run(ctx, knowledgeCommandErrorBytes, "pdfinfo", path)
	if err != nil {
		return "", err
	}
	pages, err := parsePDFPageCount(string(info))
	if err != nil {
		return "", err
	}
	if pages > limits.maxPDFPages {
		return "", fmt.Errorf(
			"scanned PDF has %d pages; limit is %d",
			pages,
			limits.maxPDFPages,
		)
	}
	tempDir, err := os.MkdirTemp("", "dreamtrans-pdf-ocr-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if removeErr := os.RemoveAll(tempDir); removeErr != nil {
			log.Printf("remove temporary PDF OCR directory: %v", removeErr)
		}
	}()
	var text boundedTextBuilder
	text.limit = limits.maxExtractedBytes
	for page := 1; page <= pages; page++ {
		outputRoot := filepath.Join(tempDir, "page")
		if _, err := run(
			ctx,
			knowledgeCommandErrorBytes,
			"pdftoppm",
			"-f",
			strconv.Itoa(page),
			"-l",
			strconv.Itoa(page),
			"-png",
			"-singlefile",
			"-scale-to",
			strconv.Itoa(knowledgePDFRenderMaxDimension),
			path,
			outputRoot,
		); err != nil {
			return "", err
		}
		imagePath := outputRoot + ".png"
		if err := validateRasterFileSize(imagePath); err != nil {
			return "", err
		}
		if err := validateKnowledgeImage(imagePath, ".png", limits.maxImagePixels); err != nil {
			return "", err
		}
		remaining := limits.maxExtractedBytes - int64(text.builder.Len())
		pageText, err := run(
			ctx,
			remaining,
			"tesseract",
			imagePath,
			"stdout",
			"-l",
			tesseractLanguageArgument(ocrLanguages),
		)
		if err != nil {
			return "", err
		}
		if err := text.WriteString(string(pageText)); err != nil {
			return "", err
		}
		if page < pages {
			if err := text.WriteByte('\n'); err != nil {
				return "", err
			}
		}
	}
	return text.String(), nil
}

func pdfNeedsOCR(text string) bool {
	return strings.TrimSpace(text) == ""
}

func parsePDFPageCount(info string) (int, error) {
	scanner := bufio.NewScanner(strings.NewReader(info))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(strings.ToLower(line), "pages:") {
			continue
		}
		value := strings.TrimSpace(line[len("Pages:"):])
		pages, err := strconv.Atoi(value)
		if err != nil || pages < 1 {
			return 0, errors.New("pdfinfo returned an invalid page count")
		}
		return pages, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("pdfinfo did not return a page count")
}

func validateRasterFileSize(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return errors.New("PDF page renderer did not produce an image")
	}
	if info.Size() < 1 || info.Size() > knowledgeRasterBytes {
		return errors.New("rendered PDF page exceeds size limit")
	}
	return nil
}

type boundedCommandBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *boundedCommandBuffer) Write(data []byte) (int, error) {
	if b.limit < 0 {
		b.limit = 0
	}
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		toWrite := int64(len(data))
		if toWrite > remaining {
			toWrite = remaining
		}
		_, _ = b.buffer.Write(data[:toWrite])
	}
	if int64(len(data)) > remaining {
		b.exceeded = true
	}
	// Report the input as consumed so os/exec keeps draining the pipe without
	// retaining data beyond the configured bound.
	return len(data), nil
}

func runBoundedKnowledgeCommand(
	parent context.Context, maxOutputBytes int64, command string, args ...string,
) ([]byte, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed on the server", command)
	}
	ctx, cancel := context.WithTimeout(parent, knowledgeCommandTimeout)
	defer cancel()
	// Both queued file extraction and synchronous LMS OCR share this process
	// budget. The timeout includes waiting, and cancellation releases waiters.
	if err := extractionCommandBudget().acquire(ctx); err != nil {
		return nil, err
	}
	defer extractionCommandBudget().release()
	var stdout boundedCommandBuffer
	stdout.limit = maxOutputBytes
	var stderr boundedCommandBuffer
	stderr.limit = knowledgeCommandErrorBytes
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // executable is fixed and file paths are generated server-side.
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if stdout.exceeded {
		return nil, errKnowledgeCommandOutputTooLarge
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s extraction timed out", command)
	}
	if runErr != nil {
		return nil, fmt.Errorf("%s extraction failed", command)
	}
	return stdout.buffer.Bytes(), nil
}

type knowledgeExtractionPool struct {
	handler  *RAGHandler
	workerID string
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	stop     chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
}

var knowledgeExtractionPools sync.Map

func (h *RAGHandler) resumeKnowledgeIndexing() {
	if h.store == nil {
		return
	}
	workers := knowledgeExtractWorkerCount()
	poolContext, poolCancel := context.WithCancel(context.Background())
	pool := &knowledgeExtractionPool{
		handler:  h,
		workerID: "knowledge-extract-" + uuid.NewString(),
		ctx:      poolContext,
		cancel:   poolCancel,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
	actual, loaded := knowledgeExtractionPools.LoadOrStore(h, pool)
	if loaded {
		actual.(*knowledgeExtractionPool).signal()
		return
	}
	for range workers {
		pool.wg.Add(1)
		go pool.worker()
	}
	pool.wg.Add(1)
	go pool.blobDeletionWorker()
}

func (h *RAGHandler) stopKnowledgeIndexing() {
	value, ok := knowledgeExtractionPools.LoadAndDelete(h)
	if !ok {
		return
	}
	pool := value.(*knowledgeExtractionPool)
	pool.once.Do(func() {
		pool.cancel()
		close(pool.stop)
	})
	pool.wg.Wait()
}

func (h *RAGHandler) enqueueKnowledgeExtraction(source *models.KnowledgeSource) {
	if source == nil || h.store == nil {
		return
	}
	value, ok := knowledgeExtractionPools.Load(h)
	if !ok {
		h.resumeKnowledgeIndexing()
		value, ok = knowledgeExtractionPools.Load(h)
	}
	if !ok {
		return
	}
	value.(*knowledgeExtractionPool).signal()
}

func (p *knowledgeExtractionPool) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *knowledgeExtractionPool) claimOne() *models.KnowledgeSource {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()
	// Use a new fencing token for every claim batch. A stable process-level
	// owner could otherwise allow an expired extractor to write through a
	// lease subsequently reacquired by this same process.
	leaseOwner := p.workerID + "-" + uuid.NewString()
	sources, err := p.handler.store.ClaimKnowledgeSourcesForExtraction(
		ctx,
		leaseOwner,
		1,
		knowledgeExtractionLease,
	)
	if err != nil || len(sources) == 0 {
		return nil
	}
	return &sources[0]
}

func (p *knowledgeExtractionPool) worker() {
	defer p.wg.Done()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		done, allowed := deployment.Default.BeginTask()
		if allowed {
			if source := p.claimOne(); source != nil {
				// Lease renewal starts immediately after this worker's own claim;
				// no leased source waits in an in-memory queue.
				p.runTask(source)
				done()
				continue
			}
			done()
		}
		select {
		case <-p.stop:
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

func (p *knowledgeExtractionPool) blobDeletionWorker() {
	defer p.wg.Done()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if p.deleteOneBlob() {
			continue
		}
		select {
		case <-p.stop:
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

func (p *knowledgeExtractionPool) runTask(source *models.KnowledgeSource) {
	ctx, cancel := context.WithTimeout(p.ctx, knowledgeTaskTimeout)
	renewDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(knowledgeExtractionLeaseRenewal)
		defer ticker.Stop()
		for {
			select {
			case <-renewDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(
					p.ctx,
					10*time.Second,
				)
				renewed, err := p.handler.store.RenewKnowledgeSourceExtractionLease(
					renewCtx,
					source.ID,
					source.ExtractLeaseOwner,
					knowledgeExtractionLease,
				)
				renewCancel()
				if err != nil || !renewed {
					cancel()
					return
				}
			}
		}
	}()
	p.handler.indexKnowledgeFile(ctx, source, source.ExtractLeaseOwner)
	close(renewDone)
	cancel()
}

func (h *RAGHandler) indexKnowledgeFile(
	ctx context.Context, source *models.KnowledgeSource, workerID string,
) {
	extension := strings.ToLower(filepath.Ext(source.BlobPath))
	text, err := extractKnowledgeText(
		ctx,
		source.BlobPath,
		extension,
		source.OCRLanguages,
	)
	if err == nil && strings.TrimSpace(text) == "" {
		err = errors.New("no readable text was found")
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		_ = h.store.FailKnowledgeSourceExtraction(
			context.WithoutCancel(ctx),
			source.ID,
			workerID,
			safeIndexError(err),
		)
		return
	}
	chunks := makeKnowledgeChunks(source, text)
	cancelledJobIDs, err := h.store.ReplaceKnowledgeChunksForExtractionAndCancelIndexJobs(
		ctx,
		source,
		chunks,
		workerID,
	)
	if err != nil {
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, store.ErrLeaseLost) {
			return
		}
		_ = h.store.FailKnowledgeSourceExtraction(
			context.WithoutCancel(ctx),
			source.ID,
			workerID,
			safeIndexError(err),
		)
		return
	}
	cancelActiveAIIndexJobs(cancelledJobIDs)
}

func (p *knowledgeExtractionPool) deleteOneBlob() bool {
	done, allowed := deployment.Default.BeginTask()
	if !allowed {
		return false
	}
	defer done()
	workerID := p.workerID + "-blob-" + uuid.NewString()
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	deletion, err := p.handler.store.ClaimKnowledgeBlobDeletion(
		ctx,
		workerID,
		knowledgeExtractionLease,
	)
	cancel()
	if err == nil && deletion != nil {
		removeErr := p.removeBlobWithBackupLock(p.ctx, deletion.BlobPath)
		finalCtx, finalCancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		if removeErr == nil {
			err = p.handler.store.CompleteKnowledgeBlobDeletion(
				finalCtx,
				deletion.ID,
				workerID,
			)
		} else {
			err = p.handler.store.FailKnowledgeBlobDeletion(
				finalCtx,
				deletion.ID,
				workerID,
				removeErr.Error(),
			)
		}
		finalCancel()
		if err != nil && !errors.Is(err, store.ErrLeaseLost) {
			log.Printf("complete knowledge blob deletion: %v", err)
		}
		return true
	}
	return false
}

// Physical deletion waits for a full backup snapshot; normal DB and audio work continue.
func (p *knowledgeExtractionPool) removeBlobWithBackupLock(parent context.Context, path string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	conn, err := p.handler.store.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock_shared(1146243412,54)`).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return errors.New("full backup is retaining knowledge files; retry deletion later")
	}
	defer func() {
		unlockCtx, finish := context.WithTimeout(context.Background(), 5*time.Second)
		defer finish()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock_shared(1146243412,54)`)
	}()
	return removeKnowledgeBlob(path)
}
