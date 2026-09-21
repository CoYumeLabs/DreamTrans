package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/dreamtrans/backend/internal/adapters/openai_provider"

	"github.com/dreamtrans/backend/internal/store"
)

func TestParseAIProjectRouteAcceptsSkillMap(t *testing.T) {
	route, status, err := parseAIProjectRoute(
		"/api/ai/projects/6f1de0f8-51d2-4c0b-9f0e-1f22a67f9a01/skill-map",
	)
	if err != nil || status != 200 {
		t.Fatalf("expected skill-map route to parse, got status=%d err=%v", status, err)
	}
	if route.Resource != "skill-map" {
		t.Fatalf("resource = %q, want skill-map", route.Resource)
	}
	if _, _, err := parseAIProjectRoute(
		"/api/ai/projects/6f1de0f8-51d2-4c0b-9f0e-1f22a67f9a01/skill-map/extra",
	); err == nil {
		t.Fatal("expected trailing path segments after skill-map to be rejected")
	}
	if _, _, err := parseAIProjectRoute(
		"/api/ai/projects/6f1de0f8-51d2-4c0b-9f0e-1f22a67f9a01/concept-map",
	); err == nil {
		t.Fatal("expected the retired concept-map route to be rejected")
	}
}

func TestParseGeneratedSkillMapToleratesFencesAndProse(t *testing.T) {
	content := "好的，这是技能地图：\n```json\n" +
		`{"skills":[{"label":"区分相关与因果","summary":"说明","outcome":"能判断"}]}` +
		"\n```\n"
	raw, err := parseGeneratedSkillMap(content)
	if err != nil {
		t.Fatalf("parse fenced output: %v", err)
	}
	if len(raw.Skills) != 1 || raw.Skills[0].Label != "区分相关与因果" {
		t.Fatalf("unexpected parse result: %+v", raw)
	}
	if _, err := parseGeneratedSkillMap("抱歉，我无法生成。"); err == nil {
		t.Fatal("expected prose-only output to be rejected")
	}
	if _, err := parseGeneratedSkillMap(`{"skills":[]}`); err == nil {
		t.Fatal("expected empty skills to be rejected")
	}
}

func testSkillMapSessions() []store.ProjectSessionRef {
	return []store.ProjectSessionRef{
		{ID: "s-1", Title: "第一讲", StartedAt: time.Now()},
		{ID: "s-2", Title: "第二讲", StartedAt: time.Now()},
	}
}

func TestBuildSkillMapDocumentResolvesEvidenceAndPrerequisites(t *testing.T) {
	raw, err := parseGeneratedSkillMap(`{
		"skills": [
			{"label": "识别相关关系", "summary": "看懂相关", "outcome": "能识别变量间的相关",
				"prerequisites": ["区分相关与因果"],
				"evidence": [
					{"session": 1, "quote": "相关不等于因果"},
					{"session": 5, "quote": "越界的会话编号"},
					{"session": 0, "quote": "非法编号"}
				]},
			{"label": "区分相关与因果", "summary": "因果推断的前提", "outcome": "能判断结论是否越界",
				"prerequisites": ["识别相关关系", "识别相关关系", "区分相关与因果", "不存在的能力"],
				"evidence": [{"session": 2, "quote": "混淆变量的例子"}]}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	doc := buildSkillMapDocument(raw, testSkillMapSessions(), nil, nil)
	if len(doc.Skills) != 2 {
		t.Fatalf("skills = %d, want 2", len(doc.Skills))
	}
	first, second := doc.Skills[0], doc.Skills[1]
	if first.ID != "s1" || second.ID != "s2" {
		t.Fatalf("unexpected ids: %q / %q", first.ID, second.ID)
	}
	// This forward reference would close a cycle with the backward reference;
	// keep the backward edge and drop only the cycle-closing edge.
	if len(first.Prerequisites) != 0 {
		t.Fatalf("forward prerequisite survived: %+v", first.Prerequisites)
	}
	if len(second.Prerequisites) != 1 || second.Prerequisites[0] != "s1" {
		t.Fatalf("prerequisites = %+v, want [s1] (dedup, no self, no unknown)", second.Prerequisites)
	}
	if len(first.Evidence) != 1 {
		t.Fatalf("evidence = %d, want 1 (out-of-range ordinals dropped)", len(first.Evidence))
	}
	if first.Evidence[0].SessionID != "s-1" || first.Evidence[0].SessionTitle != "第一讲" {
		t.Fatalf("evidence not resolved: %+v", first.Evidence[0])
	}
	// First generation: nothing is marked new.
	for _, skill := range doc.Skills {
		if skill.New {
			t.Fatal("first generation must not mark skills as new")
		}
	}
}

func TestSkillMapRebuildPreservesForwardDependencies(t *testing.T) {
	raw, err := parseGeneratedSkillMap(`{"skills":[
		{"label":"综合应用","prerequisites":["基础甲","基础乙","基础甲","综合应用","不存在"]},
		{"label":"基础乙","prerequisites":["基础甲"]},
		{"label":"基础甲","evidence":[{"source":1,"quote":"资料中的基础"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	previous := &skillMapDocument{Version: 1, Skills: []skillMapSkill{{Label: "旧能力"}, {Label: "综合应用"}}}
	doc := buildSkillMapDocument(raw, nil, []skillMapSourceRef{{ID: "source-1", Title: "教材"}}, previous)
	if len(doc.Skills) != 3 || doc.Skills[0].Label != "基础甲" || doc.Skills[1].Label != "基础乙" || doc.Skills[2].Label != "综合应用" {
		t.Fatalf("map not rebuilt in dependency order: %+v", doc.Skills)
	}
	if len(doc.Skills[2].Prerequisites) != 2 || len(doc.Skills[1].Prerequisites) != 1 {
		t.Fatalf("valid forward references lost: %+v", doc.Skills)
	}
	if doc.Skills[2].New || !doc.Skills[0].New || doc.Skills[0].Evidence[0].SourceID != "source-1" {
		t.Fatalf("name matching or source evidence lost: %+v", doc.Skills)
	}
	seen := map[string]bool{}
	for _, skill := range doc.Skills {
		for _, parent := range skill.Prerequisites {
			if !seen[parent] {
				t.Fatalf("prerequisite %s does not precede %s", parent, skill.ID)
			}
		}
		seen[skill.ID] = true
	}
}

func TestSkillMapInstructionsRebuildFromAllMaterials(t *testing.T) {
	previous := &skillMapDocument{Skills: []skillMapSkill{{Label: "旧能力"}}}
	for _, instruction := range []string{skillMapInstruction(previous), skillMapMergeInstruction(previous)} {
		if strings.Contains(instruction, "保持原样和相对顺序") || !strings.Contains(instruction, "允许重排、拆分、合并、增删") {
			t.Fatal("regeneration must not freeze the previous structure")
		}
		if !strings.Contains(instruction, `{"source":N}`) || !strings.Contains(instruction, "旧能力") {
			t.Fatal("must retain source citations and optional name matching")
		}
	}
	if !strings.Contains(skillMapChunkInstruction(), "没有转录时也必须利用资料") {
		t.Fatal("source-only chunks must be supported")
	}
}

func TestBuildSkillMapDocumentMarksNewAgainstPrevious(t *testing.T) {
	previous := &skillMapDocument{
		Version: 1,
		Skills: []skillMapSkill{
			{ID: "s1", Label: "识别相关关系"},
		},
	}
	raw, err := parseGeneratedSkillMap(`{
		"skills": [
			{"label": "识别相关关系"},
			{"label": "区分相关与因果"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	doc := buildSkillMapDocument(raw, testSkillMapSessions(), nil, previous)
	if doc.Skills[0].New {
		t.Fatal("existing skill must not be marked new")
	}
	if !doc.Skills[1].New {
		t.Fatal("added skill must be marked new")
	}
}

func TestBuildSkillMapDocumentEnforcesBounds(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(`{"skills":[`)
	for skill := 0; skill < 30; skill++ {
		if skill > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"label":"能力` + strconv.Itoa(skill) +
			strings.Repeat("长", 100) + `","summary":"` + strings.Repeat("说", 500) + `"}`)
	}
	builder.WriteString(`]}`)
	raw, err := parseGeneratedSkillMap(builder.String())
	if err != nil {
		t.Fatal(err)
	}
	doc := buildSkillMapDocument(raw, testSkillMapSessions(), nil, nil)
	if len(doc.Skills) > skillMapMaxSkills {
		t.Fatalf("skills = %d, want at most %d", len(doc.Skills), skillMapMaxSkills)
	}
	for _, skill := range doc.Skills {
		if len([]rune(skill.Label)) > skillMapMaxLabelRunes {
			t.Fatalf("label exceeds cap: %d runes", len([]rune(skill.Label)))
		}
		if len([]rune(skill.Summary)) > skillMapMaxSummaryRunes {
			t.Fatalf("summary exceeds cap: %d runes", len([]rune(skill.Summary)))
		}
	}
	// Duplicate labels collapse to one skill.
	duplicated, err := parseGeneratedSkillMap(
		`{"skills":[{"label":"识别相关关系"},{"label":"  识别相关关系  "}]}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if doc := buildSkillMapDocument(duplicated, testSkillMapSessions(), nil, nil); len(doc.Skills) != 1 {
		t.Fatalf("duplicate labels produced %d skills, want 1", len(doc.Skills))
	}
}

func TestSplitTextToBudgetCoversEveryByte(t *testing.T) {
	text := strings.Repeat("技能地图", 100)
	pieces := splitTextToBudget(text, 10)
	if len(pieces) < 2 {
		t.Fatal("expected multiple pieces for an oversize string")
	}
	if strings.Join(pieces, "") != text {
		t.Fatal("splitting must not drop or reorder bytes")
	}
	for _, piece := range pieces {
		if len(piece) > 10 && len([]rune(piece)) != 1 {
			t.Fatalf("piece %q exceeds budget without being a single rune", piece)
		}
	}
	if got := splitTextToBudget("abc", 10); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("under-budget text must stay whole, got %#v", got)
	}
}

func TestPackSkillMapChunksNeverDropsASessionTail(t *testing.T) {
	started := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sessions := []store.ProjectSessionRef{
		{ID: "s-1", Title: "第一讲", StartedAt: started},
		{ID: "s-2", Title: "第二讲", StartedAt: started.Add(24 * time.Hour)},
	}
	first := strings.Repeat("alpha ", 200)
	second := strings.Repeat("omega ", 200)
	chunks := packSkillMapChunks(skillMapMaterials(sessions, []string{first, second}, nil), 400)
	if len(chunks) < 2 {
		t.Fatalf("expected at least one chunk per oversize session, got %d", len(chunks))
	}
	var bodies strings.Builder
	for _, chunk := range chunks {
		_, body, found := strings.Cut(chunk, "\n")
		if !found {
			t.Fatalf("chunk missing session header: %q", chunk)
		}
		bodies.WriteString(body)
	}
	got := bodies.String()
	want := strings.TrimSpace(first) + strings.TrimSpace(second)
	if got != want {
		t.Fatalf("packed bodies lost or reordered text: got %d bytes, want %d", len(got), len(want))
	}
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "[会话 1]") || !strings.Contains(joined, "[会话 2]") {
		t.Fatal("session ordinals must survive packing")
	}
	if !strings.Contains(joined, "（续）") {
		t.Fatal("split sessions must keep a continuation marker")
	}
}

func TestGroupSkillMapDraftsKeepsEveryDraft(t *testing.T) {
	drafts := []*skillMapLLMOutput{{}, {}, {}}
	groups := groupSkillMapDrafts(drafts, 1)
	count := 0
	for _, group := range groups {
		count += len(group)
	}
	if count != len(drafts) {
		t.Fatalf("grouped %d drafts, want %d", count, len(drafts))
	}
	if len(groups) < 2 {
		t.Fatal("expected an oversize budget to split drafts across groups")
	}
}

func TestParseStoredSkillMapRejectsForeignContent(t *testing.T) {
	if parseStoredSkillMap("# 这是一份 markdown 摘要") != nil {
		t.Fatal("markdown content must not parse as a skill map")
	}
	if parseStoredSkillMap(`{"version":0,"skills":[]}`) != nil {
		t.Fatal("empty/versionless documents must not parse")
	}
	doc := parseStoredSkillMap(
		`{"version":1,"skills":[{"id":"s1","label":"识别相关关系"}]}`,
	)
	if doc == nil || doc.Skills[0].Label != "识别相关关系" {
		t.Fatalf("valid stored document failed to parse: %+v", doc)
	}
}

func TestSkillMapInstructionEmbedsPreviousSkeleton(t *testing.T) {
	withoutPrevious := skillMapInstruction(nil)
	if strings.Contains(withoutPrevious, "上一版技能地图") {
		t.Fatal("first generation must not mention a previous map")
	}
	previous := &skillMapDocument{
		Version: 1,
		Skills: []skillMapSkill{
			{ID: "s1", Label: "识别相关关系"},
			{ID: "s2", Label: "区分相关与因果", Prerequisites: []string{"s1"}},
		},
	}
	withPrevious := skillMapInstruction(previous)
	if !strings.Contains(withPrevious, "识别相关关系") ||
		!strings.Contains(withPrevious, "区分相关与因果") {
		t.Fatal("previous labels must appear in the skeleton")
	}
}

func TestRunSkillMapCallsKeepsOrderAndStopsOnError(t *testing.T) {
	h := &RAGHandler{}
	results, err := h.runSkillMapCalls(context.Background(), 5, func(
		_ context.Context, index int,
	) (*skillMapLLMOutput, *openai.Usage, time.Duration, error) {
		var raw skillMapLLMOutput
		if err := json.Unmarshal([]byte(`{"skills":[{"label":"`+strconv.Itoa(index)+`"}]}`), &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return &raw, &openai.Usage{TotalTokens: 1}, time.Millisecond, nil
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for index, result := range results {
		if result.raw == nil || result.raw.Skills[0].Label != strconv.Itoa(index) {
			t.Fatalf("result %d out of order: %+v", index, result.raw)
		}
	}

	boom := errors.New("boom")
	var calls atomic.Int64
	_, err = h.runSkillMapCalls(context.Background(), 20, func(
		ctx context.Context, index int,
	) (*skillMapLLMOutput, *openai.Usage, time.Duration, error) {
		calls.Add(1)
		if index == 0 {
			return nil, nil, 0, boom
		}
		<-ctx.Done()
		return nil, nil, 0, ctx.Err()
	}, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the first failure", err)
	}
	if calls.Load() > int64(skillMapMaxChunkConcurrency)+1 {
		t.Fatalf("scheduled %d calls after the first failure", calls.Load())
	}
}

func TestSkillMapMaterialsCiteSessionsAndSources(t *testing.T) {
	sessions := testSkillMapSessions()
	sources := []skillMapSourceRef{{ID: "src-1", Title: "Week 3 slides.pptx", Text: strings.Repeat("slide ", 20)}}
	chunks := packSkillMapChunks(skillMapMaterials(sessions, []string{"lecture one", "lecture two"}, sources), 100_000)
	if len(chunks) != 1 {
		t.Fatalf("small course should pack into one chunk, got %d", len(chunks))
	}
	for _, want := range []string{"[会话 1] 第一讲", "[会话 2] 第二讲", "[资料 1] Week 3 slides.pptx"} {
		if !strings.Contains(chunks[0], want) {
			t.Fatalf("chunk missing %q:\n%s", want, chunks[0])
		}
	}
	raw, err := parseGeneratedSkillMap(`{"skills":[
		{"label":"读懂效应量","outcome":"能解释 effect size","evidence":[{"source":1,"quote":"Cohen's d"},{"session":2,"quote":"we call this"},{"source":9,"quote":"nope"}]}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	doc := buildSkillMapDocument(raw, sessions, sources, nil)
	if doc.SourceCount != 1 || len(doc.Skills) != 1 || len(doc.Skills[0].Evidence) != 2 {
		t.Fatalf("doc = %+v", doc)
	}
	first, second := doc.Skills[0].Evidence[0], doc.Skills[0].Evidence[1]
	if first.SourceID != "src-1" || first.SourceTitle != "Week 3 slides.pptx" || first.SessionID != "" {
		t.Fatalf("source evidence = %+v", first)
	}
	if second.SessionID != "s-2" || second.SourceID != "" {
		t.Fatalf("session evidence = %+v", second)
	}
}
