package handlers

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
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
	text := renderDerivedText(context.Background(), &req, func(_ context.Context, png []byte, _ []string) string {
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
