package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
	"github.com/gorilla/websocket"
)

type liveStream struct {
	auth   *loginSession
	cancel context.CancelFunc
	done   chan struct{}
	stop   func()
}

// Serialize lifecycle changes per room, including the gap between stopping an
// upstream stream and committing the local ended state.
func (s *Server) beginControl(code string) (func(), error) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.controls[code] {
		return nil, fail(409, "此活动正在更新，请稍后重试")
	}
	s.controls[code] = true
	return func() { s.streamMu.Lock(); delete(s.controls, code); s.streamMu.Unlock() }, nil
}

func (s *Server) publicRoom(room Room) Room {
	if room.Transcription == "recording" {
		s.streamMu.Lock()
		active := s.streams[room.Code] != nil
		s.streamMu.Unlock()
		if !active {
			room.Transcription = "interrupted"
		}
	}
	return room.Public()
}

func uuidToken() string {
	v := token(16)
	return v[:8] + "-" + v[8:12] + "-4" + v[13:16] + "-a" + v[17:20] + "-" + v[20:]
}
func (s *Server) changeRecord(ctx context.Context, code string, change func(*Room, *storage.Record) error) error {
	for i := 0; i < 8; i++ {
		room, rec, err := s.read(ctx, code)
		if err != nil {
			return err
		}
		old := rec.Revision
		if err = change(&room, &rec); err != nil {
			return err
		}
		room.Revision = old + 1
		rec.Revision = room.Revision
		rec.Data, err = json.Marshal(room)
		if err != nil {
			return err
		}
		if err = s.store.Save(ctx, old, rec); errors.Is(err, storage.ErrConflict) {
			continue
		} else if err != nil {
			return err
		}
		s.hub.publish(code)
		return nil
	}
	return storage.ErrConflict
}

// Earlier YuAction releases used zh for Mandarin. DreamTrans and Speechmatics
// use cmn for both transcription and translation; keep saved rooms compatible.
func transcriptionLanguage(v string) string {
	if v == "zh" {
		return "cmn"
	}
	return v
}

func languageOK(v string) bool {
	if v == "cmn_en" {
		return true
	}
	if len(v) < 2 || len(v) > 12 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}
func (s *Server) prepareTranscription(w http.ResponseWriter, r *http.Request) {
	a := currentAccount(r)
	if a == nil || s.yufolo == nil {
		writeError(w, fail(401, "请先登录 Yufolo 账号"))
		return
	}
	var in struct {
		Source string `json:"sourceLanguage"`
		Target string `json:"targetLanguage"`
	}
	if err := decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	in.Source = transcriptionLanguage(in.Source)
	in.Target = transcriptionLanguage(in.Target)
	if !languageOK(in.Source) || (in.Target != "" && !languageOK(in.Target)) || in.Source == in.Target {
		writeError(w, fail(400, "请选择原文和不同的翻译语言，或关闭翻译"))
		return
	}
	code := roomCode(r)
	release, err := s.beginControl(code)
	if err != nil {
		writeError(w, err)
		return
	}
	defer release()
	err = s.changeRecord(r.Context(), code, func(room *Room, rec *storage.Record) error {
		if !s.hostAllowed(r, *rec) {
			return fail(403, "只有此活动的主持人可以开启转录")
		}
		if room.Status != "live" {
			return fail(409, "请先重新开启活动")
		}
		if rec.Link.SessionID != "" {
			rec.Link.SourceLanguage = transcriptionLanguage(rec.Link.SourceLanguage)
			rec.Link.TargetLanguage = transcriptionLanguage(rec.Link.TargetLanguage)
			// New hosts configure only the spoken language. Clearing the old
			// shared target keeps the same room/session while switching to
			// participant-selected AI translations.
			if in.Target == "" {
				rec.Link.TargetLanguage = ""
			}
			if rec.Link.SourceLanguage != in.Source || rec.Link.TargetLanguage != in.Target {
				return fail(409, "已关联会话的语言不能更改，请创建新活动")
			}
			return nil
		}
		rec.Link = storage.Link{OwnerID: a.user.ID, SessionID: uuidToken(), SourceLanguage: in.Source, TargetLanguage: in.Target}
		room.Transcription = "paused"
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	room, rec, err := s.read(r.Context(), code)
	if err != nil {
		writeError(w, err)
		return
	}
	target := rec.Link.TargetLanguage
	if target == "" {
		target = rec.Link.SourceLanguage
	}
	var result struct {
		ID string `json:"id"`
	}
	err = s.yufolo.request(r.Context(), a, "POST", "/api/sessions", map[string]string{"client_session_id": rec.Link.SessionID, "title": room.Title, "source_language": rec.Link.SourceLanguage, "target_language": target}, &result)
	if err != nil {
		writeError(w, err)
		return
	}
	if result.ID != rec.Link.SessionID {
		writeError(w, fail(502, "Yufolo 返回了不匹配的会话"))
		return
	}
	if err = s.changeRecord(r.Context(), code, func(_ *Room, rec *storage.Record) error { rec.Link.Created = true; return nil }); err != nil {
		writeError(w, err)
		return
	}
	s.transcriptionInfo(w, r)
}
func (s *Server) transcriptionInfo(w http.ResponseWriter, r *http.Request) {
	_, rec, err := s.read(r.Context(), roomCode(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if !s.hostAllowed(r, rec) {
		writeError(w, fail(403, "需要此活动的主持权限"))
		return
	}
	s.streamMu.Lock()
	active := s.streams[rec.Code] != nil
	s.streamMu.Unlock()
	respond(w, 200, map[string]any{"sessionId": rec.Link.SessionID, "sourceLanguage": transcriptionLanguage(rec.Link.SourceLanguage), "targetLanguage": transcriptionLanguage(rec.Link.TargetLanguage), "active": active, "linked": rec.Link.Created})
}
func (s *Server) archiveSegment(ctx context.Context, a *loginSession, code string, link storage.Link, seg Segment) error {
	status := "confirmed"
	if seg.Translation != "" {
		status = "translated"
	}
	body := map[string]any{"client_segment_id": seg.ID, "speaker": seg.Speaker, "text": seg.Text, "translation": seg.Translation, "start_time": seg.StartTime, "end_time": seg.EndTime, "status": status, "is_partial": false}
	if err := s.yufolo.request(ctx, a, "POST", "/api/sessions/"+link.SessionID+"/transcripts", body, nil); err != nil {
		return err
	}
	return s.changeRecord(ctx, code, func(room *Room, _ *storage.Record) error {
		for i := range room.Segments {
			if room.Segments[i].ID == seg.ID && room.Segments[i].Translation == seg.Translation {
				room.Segments[i].Archived = true
			}
		}
		return nil
	})
}
func (s *Server) endTranscription(r *http.Request, rec storage.Record) error {
	if rec.Link.SessionID == "" || !rec.Link.Created {
		return nil
	}
	a := currentAccount(r)
	if a == nil || s.yufolo == nil {
		return fail(401, "请登录 Yufolo 后结束转录")
	}
	s.streamMu.Lock()
	active := s.streams[rec.Code]
	var stop func()
	if active != nil {
		stop = active.stop
	}
	s.streamMu.Unlock()
	if active != nil {
		if stop != nil {
			stop()
		} else {
			active.cancel()
		}
	}
	if active != nil {
		select {
		case <-active.done:
		case <-r.Context().Done():
			return fail(409, "转录正在停止，请稍后重试")
		}
	}
	// A previously interrupted stream may still have durable finals awaiting
	// archival. Ending the activity must not silently leave them out of Yufolo.
	room, current, err := s.read(r.Context(), rec.Code)
	if err != nil {
		return err
	}
	for _, seg := range room.Segments {
		if seg.Source == "yufolo" && !seg.Archived && strings.HasPrefix(seg.ID, "live-") {
			if err = s.archiveSegment(r.Context(), a, rec.Code, current.Link, seg); err != nil {
				return err
			}
		}
	}
	return s.yufolo.request(r.Context(), a, "PATCH", "/api/sessions/"+rec.Link.SessionID, map[string]string{"status": "completed"}, nil)
}

type recognitionEvent struct {
	Message  string `json:"message"`
	Type     string `json:"type"`
	Reason   string `json:"reason"`
	Metadata struct {
		Text  string  `json:"transcript"`
		Start float64 `json:"start_time"`
		End   float64 `json:"end_time"`
	} `json:"metadata"`
	Results []struct {
		Text    string  `json:"content"`
		Start   float64 `json:"start_time"`
		End     float64 `json:"end_time"`
		Speaker string  `json:"speaker"`
	} `json:"results"`
}
type translatedRange struct {
	text       string
	start, end float64
}

func (s *Server) audio(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, fail(400, "需要音频 WebSocket 连接"))
		return
	}
	a := currentAccount(r)
	if a == nil || s.yufolo == nil {
		writeError(w, fail(401, "请先登录 Yufolo"))
		return
	}
	code := roomCode(r)
	room, rec, err := s.read(r.Context(), code)
	if err != nil {
		writeError(w, err)
		return
	}
	if !s.hostAllowed(r, rec) || rec.Link.OwnerID != a.user.ID {
		writeError(w, fail(403, "只有会话所有者可以采集音频"))
		return
	}
	if room.Status != "live" || !rec.Link.Created {
		writeError(w, fail(409, "请先关联一个活动中的 Yufolo 会话"))
		return
	}
	rate, err := strconv.Atoi(r.URL.Query().Get("sampleRate"))
	if err != nil || rate < 8000 || rate > 96000 {
		writeError(w, fail(400, "音频采样率不正确"))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	active := &liveStream{auth: a, cancel: cancel, done: make(chan struct{})}
	s.streamMu.Lock()
	if s.streams[code] != nil || s.controls[code] {
		s.streamMu.Unlock()
		writeError(w, fail(409, "此活动已有主持端正在转录"))
		return
	}
	s.streams[code] = active
	s.streamMu.Unlock()
	defer func() { s.streamMu.Lock(); delete(s.streams, code); close(active.done); s.streamMu.Unlock() }()
	// A lifecycle request may have completed after the first read but before
	// this stream reserved the room. Recheck before opening a paid connection.
	room, rec, err = s.read(ctx, code)
	if err != nil {
		writeError(w, err)
		return
	}
	if room.Status != "live" || !rec.Link.Created {
		writeError(w, fail(409, "活动状态已变化，请刷新后重试"))
		return
	}
	// Replay durable, unarchived finals before starting another paid stream.
	for _, seg := range room.Segments {
		if seg.Source == "yufolo" && !seg.Archived && strings.HasPrefix(seg.ID, "live-") {
			if err = s.archiveSegment(ctx, a, code, rec.Link, seg); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	if err = s.yufolo.request(ctx, a, "PATCH", "/api/sessions/"+rec.Link.SessionID, map[string]string{"status": "active"}, nil); err != nil {
		writeError(w, err)
		return
	}
	duration := rec.Link.Offset
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = s.yufolo.request(cleanup, a, "PATCH", "/api/sessions/"+rec.Link.SessionID, map[string]any{"status": "paused", "duration_seconds": int(math.Ceil(duration))}, nil)
	}()
	upstreamURL := s.yufolo.base + "/ws/speechmatics?session_id=" + url.QueryEscape(rec.Link.SessionID)
	upstreamURL = strings.Replace(strings.Replace(upstreamURL, "https://", "wss://", 1), "http://", "ws://", 1)
	a.mu.Lock()
	access := a.access
	a.mu.Unlock()
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	up, res, err := dialer.DialContext(ctx, upstreamURL, http.Header{"Authorization": []string{"Bearer " + access}})
	if err != nil {
		if res != nil {
			res.Body.Close()
		}
		writeError(w, fail(502, "Yufolo 暂时无法开启转录，请检查余额和并发限制"))
		return
	}
	defer up.Close()
	upgrader := websocket.Upgrader{CheckOrigin: sameOrigin}
	down, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer down.Close()
	// Heartbeats detect a vanished microphone browser without leaving a paid stream open.
	_ = down.SetReadDeadline(time.Now().Add(75 * time.Second))
	down.SetReadLimit(64 << 10)
	up.SetReadLimit(1 << 20)
	down.SetPongHandler(func(string) error { return down.SetReadDeadline(time.Now().Add(75 * time.Second)) })
	var outMu, upMu sync.Mutex
	send := func(value any) {
		outMu.Lock()
		defer outMu.Unlock()
		_ = down.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = down.WriteJSON(value)
	}
	var frames int
	var stopped bool
	var bytesSent uint64
	stop := func() {
		upMu.Lock()
		defer upMu.Unlock()
		if stopped {
			return
		}
		stopped = true
		_ = up.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = up.WriteJSON(map[string]any{"message": "EndOfStream", "last_seq_no": frames})
		time.AfterFunc(20*time.Second, cancel)
	}
	go func() { <-ctx.Done(); _ = up.Close(); _ = down.Close() }()
	endState := "interrupted"
	defer func() {
		cancel()
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		upMu.Lock()
		offset := rec.Link.Offset + float64(bytesSent)/float64(rate*2)
		upMu.Unlock()
		duration = offset
		_ = s.changeRecord(cleanup, code, func(room *Room, row *storage.Record) error {
			if offset > row.Link.Offset {
				row.Link.Offset = offset
			}
			room.Transcription = endState
			return nil
		})
	}()
	config := map[string]any{"message": "StartRecognition", "audio_format": map[string]any{"type": "raw", "encoding": "pcm_s16le", "sample_rate": rate}, "transcription_config": map[string]any{"language": transcriptionLanguage(rec.Link.SourceLanguage), "enable_partials": true, "max_delay": 2}}
	if rec.Link.TargetLanguage != "" {
		config["translation_config"] = map[string]any{"target_languages": []string{transcriptionLanguage(rec.Link.TargetLanguage)}, "enable_partials": false}
	}
	upMu.Lock()
	err = up.WriteJSON(config)
	upMu.Unlock()
	if err != nil {
		return
	}
	s.streamMu.Lock()
	active.stop = stop
	s.streamMu.Unlock()
	go func() {
		for {
			kind, data, e := down.ReadMessage()
			if e != nil {
				cancel()
				return
			}
			if kind == websocket.TextMessage {
				var cmd struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(data, &cmd) != nil || cmd.Type != "stop" {
					cancel()
					return
				}
				stop()
				continue
			}
			if kind != websocket.BinaryMessage || len(data)%2 != 0 {
				cancel()
				return
			}
			upMu.Lock()
			if stopped {
				upMu.Unlock()
				continue
			}
			_ = up.SetWriteDeadline(time.Now().Add(10 * time.Second))
			e = up.WriteMessage(websocket.BinaryMessage, data)
			if e == nil {
				frames++
				bytesSent += uint64(len(data))
			}
			upMu.Unlock()
			if e != nil {
				cancel()
				return
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := down.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					cancel()
					return
				}
				var profile struct {
					User accountUser `json:"user"`
				}
				if err := s.yufolo.request(ctx, a, "GET", "/api/user/profile", nil, &profile); err != nil || profile.User.ID != a.user.ID {
					send(map[string]string{"type": "error", "message": "账号连接已失效，请重新登录"})
					cancel()
					return
				}
			}
		}
	}()
	streamID := token(12)
	seen := map[string]bool{}
	pending := []translatedRange{}
	applyTranslation := func(tr translatedRange) (bool, error) {
		var updated Segment
		matched := false
		err := s.changeRecord(ctx, code, func(room *Room, _ *storage.Record) error {
			best := -1
			overlap := float64(0)
			for i, seg := range room.Segments {
				if !strings.HasPrefix(seg.ID, "live-"+streamID+"-") {
					continue
				}
				score := math.Min(seg.EndTime, tr.end) - math.Max(seg.StartTime, tr.start)
				if score > overlap {
					overlap = score
					best = i
				}
			}
			if best < 0 {
				return nil
			}
			matched = true
			seg := &room.Segments[best]
			if seg.Translation != "" {
				seg.Translation += " "
			}
			seg.Translation += tr.text
			seg.Archived = false
			updated = *seg
			return nil
		})
		if err != nil || !matched {
			return matched, err
		}
		return true, s.archiveSegment(ctx, a, code, rec.Link, updated)
	}
	for {
		_, data, e := up.ReadMessage()
		if e != nil {
			if ctx.Err() == nil {
				endState = "interrupted"
				send(map[string]string{"type": "error", "message": "转录连接已断开，请重新开始；已确认字幕已保留"})
			}
			return
		}
		var event recognitionEvent
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		switch event.Message {
		case "RecognitionStarted":
			if err = s.changeRecord(ctx, code, func(room *Room, _ *storage.Record) error {
				if room.Status != "live" {
					return fail(409, "活动已结束")
				}
				room.Transcription = "recording"
				return nil
			}); err != nil {
				return
			}
			send(map[string]string{"type": "ready"})
		case "AddTranscript":
			text := normalizeSegmentText(event.Metadata.Text)
			start, end := event.Metadata.Start+rec.Link.Offset, event.Metadata.End+rec.Link.Offset
			if text == "" || end < start || start < 0 {
				continue
			}
			id := "live-" + streamID + "-" + digest(fmt.Sprintf("%f:%f:%s", start, end, text))[:20]
			if seen[id] {
				continue
			}
			seen[id] = true
			seg := Segment{ID: id, Text: text, Source: "yufolo", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Parts: 1, StartTime: start, EndTime: end, Speaker: "Speaker"}
			incoming := seg
			err = s.changeRecord(ctx, code, func(room *Room, row *storage.Record) error {
				seg = incoming
				if room.Status != "live" {
					return fail(409, "活动已结束")
				}
				if n := len(room.Segments); n > 0 && strings.HasPrefix(room.Segments[n-1].ID, "live-"+streamID+"-") && canMergeSegment(room.Segments[n-1], seg) {
					previous := room.Segments[n-1]
					previous.Text = joinSegmentText(previous.Text, seg.Text)
					previous.EndTime = math.Max(previous.EndTime, end)
					previous.UpdatedAt = seg.UpdatedAt
					previous.Parts++
					previous.Archived = false
					previous.Translations = nil
					previous.TranslationErrors = nil
					seg = previous
					room.Segments[n-1] = seg
				} else {
					if len(room.Segments) >= 5000 {
						return fail(409, "活动字幕已达到容量上限")
					}
					room.Segments = append(room.Segments, seg)
				}
				if end > row.Link.Offset {
					row.Link.Offset = end
				}
				return nil
			})
			if err == nil {
				err = s.archiveSegment(ctx, a, code, rec.Link, seg)
			}
			if err == nil {
				remaining := pending[:0]
				for _, tr := range pending {
					matched, e := applyTranslation(tr)
					if e != nil {
						err = e
						break
					}
					if !matched {
						remaining = append(remaining, tr)
					}
				}
				pending = remaining
			}
		case "AddTranslation":
			for _, result := range event.Results {
				tr := translatedRange{strings.TrimSpace(result.Text), result.Start + rec.Link.Offset, result.End + rec.Link.Offset}
				if tr.text == "" || tr.end < tr.start {
					continue
				}
				key := fmt.Sprintf("translation:%f:%f:%s", tr.start, tr.end, tr.text)
				if seen[key] {
					continue
				}
				seen[key] = true
				var matched bool
				matched, err = applyTranslation(tr)
				if err != nil {
					break
				}
				if !matched {
					if len(pending) >= 1000 {
						err = fail(409, "译文等待队列已满")
						break
					}
					pending = append(pending, tr)
				}
			}
		case "Error":
			endState = "error"
			send(map[string]string{"type": "error", "message": "Yufolo：" + event.Reason})
			return
		case "EndOfTranscript":
			endState = "paused"
			send(map[string]string{"type": "stopped"})
			return
		}
		if err != nil {
			endState = "error"
			send(map[string]string{"type": "error", "message": "字幕同步中断，已收到的原文保留在房间；重新开始会补存到 Yufolo"})
			return
		}
	}
}
