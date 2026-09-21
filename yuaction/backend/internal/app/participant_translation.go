package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
	"github.com/gorilla/websocket"
)

var captionLanguages = map[string]string{"cmn": "中文", "en": "English", "ja": "日本語", "ko": "한국어", "de": "Deutsch", "fr": "Français", "es": "Español"}

func (s *Server) requestTranslations(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Language   string   `json:"language"`
		Retry      bool     `json:"retry"`
		SegmentIDs []string `json:"segmentIds"`
	}
	if err := decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	in.Language = transcriptionLanguage(in.Language)
	if captionLanguages[in.Language] == "" {
		writeError(w, fail(400, "请选择支持的译文语言"))
		return
	}
	code := roomCode(r)
	room, rec, err := s.read(r.Context(), code)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(in.SegmentIDs) > 50 {
		writeError(w, fail(400, "请求的字幕过多"))
		return
	}
	selected := make(map[string]bool, len(in.SegmentIDs))
	for _, id := range in.SegmentIDs {
		selected[id] = true
	}
	if s.yufolo == nil || !rec.Link.Created {
		writeError(w, fail(409, "主持人连接 Yufolo 转录后可选择译文"))
		return
	}
	s.streamMu.Lock()
	var a *loginSession
	if stream := s.streams[code]; stream != nil {
		a = stream.auth
	}
	s.streamMu.Unlock()
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	if a == nil {
		a = s.aiHosts[code]
	}
	if a == nil || a.user.ID != rec.Link.OwnerID {
		writeError(w, fail(503, "主持人需要登录并保持工作台在线，才能生成新译文"))
		return
	}
	started := 0
	window := 8
	if len(selected) > 0 {
		window = 50
	}
	for i := max(0, len(room.Segments)-window); i < len(room.Segments); i++ {
		seg := room.Segments[i]
		if len(selected) > 0 && !selected[seg.ID] {
			continue
		}
		if seg.Source != "yufolo" || seg.Translations[in.Language] != "" {
			continue
		}
		if seg.TranslationErrors[in.Language] != "" && !in.Retry {
			continue
		}
		// A live final may still grow as micro-finals arrive. Do not translate
		// "Could you" and "hear me?" separately or bill every revision.
		if i == len(room.Segments)-1 && room.Transcription == "recording" && time.Since(seg.UpdatedAt) < 4*time.Second {
			continue
		}
		key := jobKey(code, "translation", seg.ID+":"+in.Language)
		if s.aiJobs[key] != nil || len(s.aiJobs) >= 4 {
			continue
		}
		if !s.allow(code+":translation", 30) {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		s.aiJobs[key] = cancel
		started++
		go func(seg Segment, target, key string) {
			defer cancel()
			text := ""
			var translationErr error
			if transcriptionLanguage(rec.Link.SourceLanguage) == target {
				text = seg.Text
			} else {
				text, translationErr = s.translateYufoloSegment(ctx, a, rec.Link, seg, target)
			}
			s.aiMu.Lock()
			defer s.aiMu.Unlock()
			defer delete(s.aiJobs, key)
			cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = s.changeRecord(cleanup, code, func(next *Room, _ *storage.Record) error {
				for j := range next.Segments {
					current := &next.Segments[j]
					if current.ID != seg.ID {
						continue
					}
					if current.Text != seg.Text {
						return nil
					}
					if current.Translations == nil {
						current.Translations = map[string]string{}
					}
					if current.TranslationErrors == nil {
						current.TranslationErrors = map[string]string{}
					}
					if translationErr != nil {
						current.TranslationErrors[target] = "译文暂不可用，请稍后重试"
					} else {
						current.Translations[target] = text
						delete(current.TranslationErrors, target)
					}
					break
				}
				return nil
			})
		}(seg, in.Language, key)
	}
	respond(w, 202, map[string]int{"started": started})
}

// Reuse Yufolo's atomic AI translator, model selection and billing.
// Atomic request IDs identify cached/replayed work; captions never expose JWTs.
func (s *Server) translateYufoloSegment(ctx context.Context, a *loginSession, link storage.Link, seg Segment, target string) (string, error) {
	access, err := s.yufolo.accessToken(ctx, a, "")
	if err != nil {
		return "", err
	}
	address := strings.Replace(strings.Replace(s.yufolo.base, "https://", "wss://", 1), "http://", "ws://", 1) + "/ws/translate"
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	socket, res, err := dialer.DialContext(ctx, address, http.Header{"Authorization": []string{"Bearer " + access}})
	if err != nil {
		if res != nil {
			res.Body.Close()
		}
		return "", err
	}
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	socket.SetReadLimit(1 << 20)
	_ = socket.SetReadDeadline(time.Now().Add(140 * time.Second))
	_ = socket.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err = socket.WriteJSON(map[string]any{"type": "init", "mode": "ai_rolling", "config": map[string]any{"session_id": link.SessionID, "source_language": link.SourceLanguage, "target_language": target, "min_chunk_chars": 1, "disable_summarization": true, "disable_embeddings": true}}); err != nil {
		return "", err
	}
	requestID := "yuaction-" + digest(link.SessionID + ":" + seg.ID + ":" + seg.Text + ":" + target)[:48]
	ready := false
	for {
		var event struct {
			Message      string `json:"message"`
			Reason       string `json:"reason"`
			Capabilities struct {
				RequestIDs bool `json:"request_ids"`
				Atomic     bool `json:"atomic_transcripts"`
			} `json:"capabilities"`
			Results []struct {
				ID   string `json:"request_id"`
				Text string `json:"content"`
			} `json:"results"`
		}
		if err = socket.ReadJSON(&event); err != nil {
			return "", err
		}
		switch event.Message {
		case "Info":
			if event.Reason != "translator initialized" || ready {
				continue
			}
			if !event.Capabilities.RequestIDs || !event.Capabilities.Atomic {
				return "", fmt.Errorf("Yufolo 需要支持原子翻译请求")
			}
			ready = true
			if err = socket.WriteJSON(map[string]any{"type": "transcript", "payload": map[string]any{"request_id": requestID, "speaker": seg.Speaker, "transcript": seg.Text, "start_time": seg.StartTime, "end_time": seg.EndTime}}); err != nil {
				return "", err
			}
		case "AddTranslation":
			for _, result := range event.Results {
				if result.ID == requestID && strings.TrimSpace(result.Text) != "" {
					return strings.TrimSpace(result.Text), nil
				}
			}
		case "Error":
			return "", fmt.Errorf("Yufolo 翻译失败")
		}
	}
}
