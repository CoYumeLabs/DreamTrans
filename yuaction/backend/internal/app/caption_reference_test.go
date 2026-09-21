package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
)

func TestCaptionQuoteKeepsAllSourceIdentities(t *testing.T) {
	h := New(storage.NewMemory(), Config{Demo: true}).Handler()
	a := createRoom(t, h, "Caption references")
	base := "/api/rooms/" + a.Room.Code
	for i, text := range []string{"Could you", "hear me", "?"} {
		request(t, h, "POST", base+"/demo-segments", a.Key, map[string]string{"id": fmt.Sprint(i), "text": text}, 200)
	}
	request(t, h, "POST", base+"/questions", "", map[string]any{"content": "Explain", "segmentId": "0", "segmentIds": []string{"0", "1", "2"}}, 201)
	r := readRoom(t, h, a.Room.Code)
	if r.Questions[0].QuotedText != "Could you hear me?" || len(r.Questions[0].SegmentIDs) != 3 || len(r.Segments) != 3 {
		t.Fatalf("quote lost fragments or rewrote history: %+v", r)
	}
	for _, ids := range [][]string{{"0", "missing"}, {"0", "0"}, {"1", "2"}} {
		request(t, h, "POST", base+"/questions", "", map[string]any{"content": "Invalid", "segmentId": "0", "segmentIds": ids}, 400)
	}
	request(t, h, "POST", base+"/questions", "", map[string]string{"content": "Old client", "segmentId": "1"}, 201)
	if got := readRoom(t, h, a.Room.Code).Questions[1].QuotedText; got != "hear me" {
		t.Fatal(got)
	}
}

func TestVisibleCaptionCanTranslateFragmentsBeforeLastEight(t *testing.T) {
	f := newFakeYufolo(t)
	s := New(storage.NewMemory(), Config{YufoloURL: f.server.URL})
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	teacher := integrationClient(t, server.URL, "teacher@example.com")
	room := integratedRoom(t, teacher, server.URL)
	base := server.URL + "/api/rooms/" + room.Room.Code
	httpJSON(t, teacher, base+"/assistant", "GET", nil, 200)
	httpJSON(t, teacher, base+"/transcription", "POST", map[string]string{"sourceLanguage": "en"}, 200)
	if err := s.changeRecord(context.Background(), room.Room.Code, func(r *Room, _ *storage.Record) error {
		for i := 0; i < 12; i++ {
			r.Segments = append(r.Segments, Segment{ID: fmt.Sprint(i), Text: fmt.Sprintf("word %d", i), Source: "yufolo", StartTime: float64(i), EndTime: float64(i + 1)})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"language": "cmn", "segmentIds": []string{"0"}}
	httpJSON(t, http.DefaultClient, base+"/translations", "POST", body, 202)
	eventually(t, func() bool {
		r := readRoom(t, s.Handler(), room.Room.Code)
		return r.Segments[0].Translations["cmn"] != ""
	})
	httpJSON(t, http.DefaultClient, base+"/translations", "POST", body, 202)
	if f.translations.Load() != 1 {
		t.Fatal("visible caption translation charged twice")
	}
	var r Room
	_ = json.Unmarshal(httpJSON(t, http.DefaultClient, base, "GET", nil, 200), &r)
	if r.Segments[1].Translations["cmn"] != "" {
		t.Fatal("translated unrequested fragment")
	}
}
