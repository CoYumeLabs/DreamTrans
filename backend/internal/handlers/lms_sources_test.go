package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/rag"
)

func TestParseAIProjectRouteAcceptsDerivedSources(t *testing.T) {
	route, status, err := parseAIProjectRoute(
		"/api/ai/projects/6f1de0f8-51d2-4c0b-9f0e-1f22a67f9a01/sources/derived",
	)
	if err != nil || status != 200 || route.Resource != "sources" || route.Action != "derived" {
		t.Fatalf("sources/derived: status=%d err=%v route=%+v", status, err, route)
	}
	route, status, err = parseAIProjectRoute(
		"/api/ai/projects/6f1de0f8-51d2-4c0b-9f0e-1f22a67f9a01/sources/0b0d5c7e-7f7a-4a53-9c0f-3f3c0e1b2a10/original",
	)
	if err != nil || status != 200 || route.Action != "original" || route.ResourceID != "0b0d5c7e-7f7a-4a53-9c0f-3f3c0e1b2a10" {
		t.Fatalf("sources/{id}/original: status=%d err=%v route=%+v", status, err, route)
	}
	if _, _, err := parseAIProjectRoute(
		"/api/ai/projects/6f1de0f8-51d2-4c0b-9f0e-1f22a67f9a01/sources/not-a-uuid",
	); err == nil {
		t.Fatal("non-uuid source ids are still rejected")
	}
}

func validDerivedRequest() derivedSourceRequest {
	return derivedSourceRequest{
		SHA256:    strings.Repeat("ab", 32),
		Filename:  "Week6_slides.pdf",
		MediaType: "application/pdf",
		SizeBytes: 1234,
		Pages: []derivedPage{
			{N: 1, Text: "Correlation is not causation."},
			{N: 2, Text: "", Figures: []derivedFigure{{PNGBase64: base64.StdEncoding.EncodeToString([]byte("png"))}}},
		},
		LMS: derivedLMS{
			Host: "LMS.Monash.edu", CourseID: 42, CourseShortname: "PSY2041",
			Section: "Week 6", CMID: 777, ModType: "Resource", ModuleName: "Lecture 6 slides",
			TimeModified: 1_725_000_000,
		},
	}
}

func TestValidateDerivedSourceBoundsAndNormalizes(t *testing.T) {
	req := validDerivedRequest()
	if err := validateDerivedSource(&req); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if req.LMS.Host != "lms.monash.edu" || req.LMS.ModType != "resource" || req.PageCount != 2 {
		t.Fatalf("normalization: %+v", req.LMS)
	}
	bad := validDerivedRequest()
	bad.SHA256 = "nope"
	if err := validateDerivedSource(&bad); err == nil {
		t.Fatal("sha256 must be validated")
	}
	bad = validDerivedRequest()
	bad.Pages = nil
	if err := validateDerivedSource(&bad); err == nil {
		t.Fatal("pages are required")
	}
	bad = validDerivedRequest()
	bad.LMS.CMID = 0
	if err := validateDerivedSource(&bad); err == nil {
		t.Fatal("cmid is required")
	}
	bad = validDerivedRequest()
	bad.Pages[0].Text = strings.Repeat("x", derivedMaxTextRunes+1)
	if err := validateDerivedSource(&bad); err == nil {
		t.Fatal("text must be bounded")
	}
}

func TestRenderDerivedTextKeepsPagesAndDropsRenders(t *testing.T) {
	req := validDerivedRequest()
	if err := validateDerivedSource(&req); err != nil {
		t.Fatal(err)
	}
	calls := 0
	text := renderDerivedText(context.Background(), &req, func(_ context.Context, png []byte, _ string) string {
		calls++
		if string(png) != "png" {
			t.Fatalf("ocr got %q", png)
		}
		return "Figure: scatter plot of coffee vs GPA"
	})
	for _, want := range []string{"## 第 1 页", "Correlation is not causation.", "## 第 2 页", "[图 1] Figure: scatter plot"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered text missing %q:\n%s", want, text)
		}
	}
	if calls != 1 {
		t.Fatalf("ocr calls = %d, want 1", calls)
	}
	if req.Pages[1].Figures[0].PNGBase64 != "" {
		t.Fatal("figure renders must be dropped after OCR")
	}
	if name := derivedSourceName(&req); name != "Week 6 · Week6_slides.pdf" {
		t.Fatalf("source name = %q", name)
	}
	// No OCR available: page text still lands, figure-only pages vanish.
	req = validDerivedRequest()
	_ = validateDerivedSource(&req)
	plain := renderDerivedText(context.Background(), &req, nil)
	if strings.Contains(plain, "第 2 页") || !strings.Contains(plain, "第 1 页") {
		t.Fatalf("without OCR only text pages remain:\n%s", plain)
	}
}

func TestRenderDerivedTextReadsFiguresInParallelAndKeepsOrder(t *testing.T) {
	req := validDerivedRequest()
	req.Pages = nil
	for n := 1; n <= 18; n++ {
		req.Pages = append(req.Pages, derivedPage{N: n, Text: fmt.Sprintf("page %d text", n), Figures: []derivedFigure{
			{PNGBase64: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("fig-%d", n)))},
		}})
	}
	var active, peak int32
	started := time.Now()
	text := renderDerivedText(context.Background(), &req, func(_ context.Context, png []byte, pageText string) string {
		now := atomic.AddInt32(&active, 1)
		defer atomic.AddInt32(&active, -1)
		for {
			seen := atomic.LoadInt32(&peak)
			if now <= seen || atomic.CompareAndSwapInt32(&peak, seen, now) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		return "read " + string(png) + " beside " + pageText
	})
	if peak < 2 || peak > derivedFigureConcurrency {
		t.Fatalf("peak concurrency = %d, want 2..%d", peak, derivedFigureConcurrency)
	}
	if elapsed := time.Since(started); elapsed > 18*40*time.Millisecond/2 {
		t.Fatalf("figures were not read in parallel: %v", elapsed)
	}
	last := -1
	for n := 1; n <= 18; n++ {
		want := fmt.Sprintf("## 第 %d 页\npage %d text\n[图 1] read fig-%d beside page %d text", n, n, n, n)
		at := strings.Index(text, want)
		if at <= last {
			t.Fatalf("page %d missing or out of order:\n%s", n, text)
		}
		last = at
	}
	for _, page := range req.Pages {
		if page.Figures[0].PNGBase64 != "" {
			t.Fatal("renders must be dropped")
		}
	}
}

func TestDerivedFigureReaderFallsBackAndStopsOnEmptyBalance(t *testing.T) {
	var stats derivedFigureStats
	describeCalls, fallbackCalls := 0, 0
	answers := []error{nil, rag.ErrFigureNothingToAdd, errors.New("provider 500"), fmt.Errorf("%w: reserve", errRAGPaymentRequired)}
	reader := newDerivedFigureReader("gpt-5.6-luna",
		func(_ context.Context, model string, _ []byte, _ string) (string, error) {
			if model != "gpt-5.6-luna" {
				t.Fatalf("model %q", model)
			}
			err := answers[describeCalls]
			describeCalls++
			if err == nil {
				return "a scatter plot", nil
			}
			return "", err
		},
		(&RAGHandler{}).isRAGAccountingError,
		func(context.Context, []byte, string) string { fallbackCalls++; return "ocr text" },
		&stats,
	)
	got := []string{}
	for i := 0; i < 6; i++ {
		got = append(got, reader(context.Background(), []byte("png"), "page"))
	}
	want := []string{"a scatter plot", "", "ocr text", "ocr text", "ocr text", "ocr text"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("reads = %q, want %q", got, want)
	}
	if describeCalls != 4 {
		t.Fatalf("model kept being called after the balance ran out: %d calls", describeCalls)
	}
	if stats.Vision != 2 || stats.OCR != 4 || stats.VisionStopped != "insufficient_balance" || fallbackCalls != 4 {
		t.Fatalf("stats = %+v fallback=%d", stats, fallbackCalls)
	}
	off := newDerivedFigureReader("", func(context.Context, string, []byte, string) (string, error) {
		t.Fatal("model called while disabled")
		return "", nil
	}, (&RAGHandler{}).isRAGAccountingError, func(context.Context, []byte, string) string { return "ocr" }, &derivedFigureStats{})
	if off(context.Background(), []byte("png"), "") != "ocr" {
		t.Fatal("MOODLE_FIGURE_MODEL=off must use tesseract")
	}
}

func TestFigureOCRLanguagesSkipsChineseForEnglishPages(t *testing.T) {
	if got := figureOCRLanguages("Reliability refers to the consistency of a measure over time"); len(got) != 1 || got[0] != "eng" {
		t.Fatalf("english page: %v", got)
	}
	for _, text := range []string{"", "信度是指测量结果的一致性 reliability", "short"} {
		if got := figureOCRLanguages(text); len(got) != 2 {
			t.Fatalf("%q: %v", text, got)
		}
	}
}
