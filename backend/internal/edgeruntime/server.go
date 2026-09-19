package edgeruntime

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Config struct {
	NodeID, PublicKey, ProviderKey, ProviderURL, Version string
	Origins                                              []string
	Maximum                                              int
	Training                                             bool
}
type Server struct {
	config              Config
	key                 ed25519.PublicKey
	main                *MainClient
	queue               *Queue
	mu                  sync.Mutex
	connections         map[string]*stream
	enabled             atomic.Bool
	providerHealthy     atomic.Bool
	providerLatency     atomic.Int64
	successes, failures atomic.Int64
}
type stream struct {
	mu                                 sync.Mutex
	write                              sync.Mutex
	client, provider                   *websocket.Conn
	grant                              edgeprotocol.Grant
	samples, providerSamples, audioSeq int64
	done                               chan struct{}
	once                               sync.Once
	started                            time.Time
}

func New(config *Config, main *MainClient, queue *Queue) (*Server, error) {
	key, err := base64.RawStdEncoding.DecodeString(config.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid main-site verification key")
	}
	if config.Maximum < 1 || config.Maximum > 4096 || len(config.Origins) == 0 || config.ProviderKey == "" {
		return nil, errors.New("edge capacity, origins and independent provider key are required")
	}
	if config.ProviderURL == "" {
		config.ProviderURL = "wss://global.rt.speechmatics.com/v2"
	}
	s := &Server{config: *config, key: ed25519.PublicKey(key), main: main, queue: queue, connections: make(map[string]*stream)}
	deployment.Default.SetPending(queue.Pending)
	return s, nil
}
func (s *Server) origin(origin string) bool {
	for _, allowed := range s.config.Origins {
		if allowed == origin && origin != "" {
			return true
		}
	}
	return false
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.providerHealthy.Load() {
			http.Error(w, "provider not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		if !s.origin(r.Header.Get("Origin")) {
			http.Error(w, "origin rejected", http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(204)
	})
	mux.HandleFunc("/ws/edge", s.accept)
	return deployment.Default.Middleware(mux)
}
func (st *stream) send(value any) error {
	st.write.Lock()
	defer st.write.Unlock()
	_ = st.client.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return st.client.WriteJSON(value)
}
func (st *stream) stop() {
	st.once.Do(func() {
		close(st.done)
		_ = st.client.Close()
		if st.provider != nil {
			_ = st.provider.Close()
		}
	})
}
func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	if !s.enabled.Load() {
		http.Error(w, "edge not accepting sessions", http.StatusServiceUnavailable)
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return s.origin(r.Header.Get("Origin")) }, Subprotocols: []string{"dreamtrans-edge-v1"}, HandshakeTimeout: 10 * time.Second}
	client, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	client.SetReadLimit(edgeprotocol.MaxAudioBytes + 1024)
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	var auth struct {
		Token string `json:"token"`
	}
	if err := client.ReadJSON(&auth); err != nil {
		return
	}
	grant, err := edgeprotocol.Verify(s.key, auth.Token, s.config.NodeID, r.Header.Get("Origin"))
	if err != nil || grant.Training != s.config.Training {
		return
	}
	st := &stream{client: client, grant: *grant, done: make(chan struct{}), started: time.Now()}
	s.mu.Lock()
	if len(s.connections) >= s.config.Maximum || s.connections[grant.SessionID] != nil {
		s.mu.Unlock()
		return
	}
	s.connections[grant.SessionID] = st
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.connections, grant.SessionID); s.mu.Unlock() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var confirmed edgeprotocol.Authorization
	if err := s.main.Call(ctx, "connect", map[string]string{"token_id": grant.ID}, &confirmed); err != nil {
		_ = st.send(map[string]string{"message": "Error", "reason": "Session authorization unavailable"})
		return
	}
	// Verify the main response cryptographically too; a grant cannot grow in a JSON-only response.
	authoritative, err := edgeprotocol.Verify(s.key, confirmed.Token, s.config.NodeID, r.Header.Get("Origin"))
	if err != nil {
		return
	}
	st.grant = *authoritative
	start := time.Now()
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	provider, response, err := dialer.DialContext(ctx, s.config.ProviderURL, http.Header{"Authorization": []string{"Bearer " + s.config.ProviderKey}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		s.failures.Add(1)
		_ = s.event(st, "end", nil)
		return
	}
	s.providerLatency.Store(time.Since(start).Milliseconds())
	s.successes.Add(1)
	st.provider = provider
	defer st.stop()
	var startMessage map[string]any
	if err := client.ReadJSON(&startMessage); err != nil {
		_ = s.event(st, "end", nil)
		return
	}
	if !validateStart(startMessage, st.grant.SampleRate) {
		_ = s.event(st, "end", nil)
		return
	}
	delete(startMessage, "translation_config") // phase one: translations remain on the main AI WebSocket
	if err := provider.WriteJSON(startMessage); err != nil {
		_ = s.event(st, "end", nil)
		return
	}
	ended := make(chan struct{})
	go func() { defer close(ended); s.readProvider(st) }()
	controlDone := make(chan struct{})
	go func() { defer close(controlDone); s.controlStream(ctx, st) }()
	s.readAudio(st)
	cancel()
	st.stop()
	<-ended
	<-controlDone
	if err := s.event(st, "end", nil); err != nil {
		log.Printf("edge session=%s end event pending: journal full or unavailable", grant.SessionID)
	}
}
func validateStart(m map[string]any, rate int) bool {
	if m["message"] != "StartRecognition" {
		return false
	}
	audio, ok := m["audio_format"].(map[string]any)
	if !ok {
		return false
	}
	sr, ok := audio["sample_rate"].(float64)
	if !ok || int(sr) != rate || audio["encoding"] != "pcm_f32le" || audio["type"] != "raw" {
		return false
	}
	// Prevent client-controlled credentials/urls/custom upstream operations.
	for k := range m {
		if k != "message" && k != "audio_format" && k != "transcription_config" && k != "translation_config" {
			delete(m, k)
		}
	}
	return true
}
func (s *Server) readAudio(st *stream) {
	_ = st.client.SetReadDeadline(time.Now().Add(60 * time.Second))
	st.client.SetPongHandler(func(string) error { return st.client.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	for {
		kind, data, err := st.client.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.TextMessage {
			var msg map[string]any
			if json.Unmarshal(data, &msg) != nil {
				return
			}
			if msg["message"] == "EndOfStream" {
				st.mu.Lock()
				last := st.audioSeq
				st.mu.Unlock()
				_ = st.provider.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_ = st.provider.WriteJSON(map[string]any{"message": "EndOfStream", "last_seq_no": last})
				select {
				case <-st.done:
				case <-time.After(15 * time.Second):
				}
				return
			}
			continue
		}
		if kind != websocket.BinaryMessage || len(data) < 12 || len(data) > edgeprotocol.MaxAudioBytes+8 || (len(data)-8)%4 != 0 {
			return
		}
		wireSequence := binary.BigEndian.Uint64(data[:8])
		if wireSequence > 1_000_000_000 {
			return
		}
		sequence := int64(wireSequence)
		samples := int64((len(data) - 8) / 4)
		st.mu.Lock()
		if sequence <= st.audioSeq {
			st.mu.Unlock()
			continue
		}
		if sequence != st.audioSeq+1 || time.Now().After(st.grant.ExpiresAt.Add(-2*time.Second)) || st.samples+samples > st.grant.ApprovedSamples {
			st.mu.Unlock()
			_ = st.send(map[string]string{"message": "Error", "type": "edge_authorization_limit", "reason": "Edge: 已批准的额度或连接授权已到期，录音已停止；请恢复连接后继续。"})
			return
		}
		// Serialize upstream audio writes. No main-site request occurs in this loop.
		_ = st.provider.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := st.provider.WriteMessage(websocket.BinaryMessage, data[8:]); err != nil {
			st.mu.Unlock()
			return
		}
		st.audioSeq = sequence
		st.samples += samples
		st.providerSamples += samples
		st.mu.Unlock()
		// This only acknowledges receipt, never permanent transcript storage.
		if err := st.send(map[string]any{"message": "EdgeReceived", "sequence": sequence}); err != nil {
			return
		}
	}
}
func (s *Server) event(st *stream, kind string, t *edgeprotocol.Transcript) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	e := edgeprotocol.Event{SessionID: st.grant.SessionID, Generation: st.grant.Generation, EventID: uuid.NewString(), Kind: kind, AudioSequence: st.audioSeq, Samples: st.samples, ProviderSamples: st.providerSamples, Transcript: t}
	return s.queue.Append(&e)
}
func (s *Server) readProvider(st *stream) {
	st.mu.Lock()
	offset, generation := st.grant.TimelineOffset, st.grant.Generation
	st.mu.Unlock()
	defer st.stop()
	st.provider.SetReadLimit(256 * 1024)
	for {
		_, raw, err := st.provider.ReadMessage()
		if err != nil {
			return
		}
		var msg map[string]any
		if json.Unmarshal(raw, &msg) != nil {
			return
		}
		if msg["message"] == "AddTranscript" {
			metadata, ok := msg["metadata"].(map[string]any)
			if !ok {
				return
			}
			text, _ := metadata["transcript"].(string)
			start, _ := metadata["start_time"].(float64)
			end, _ := metadata["end_time"].(float64)
			start += offset
			end += offset
			metadata["start_time"] = start
			metadata["end_time"] = end
			msg["edge_absolute_time"] = true
			id := uuid.NewString()
			speaker := "Speaker"
			if results, ok := msg["results"].([]any); ok && len(results) > 0 {
				if result, ok := results[0].(map[string]any); ok {
					if alternatives, ok := result["alternatives"].([]any); ok && len(alternatives) > 0 {
						if alternative, ok := alternatives[0].(map[string]any); ok {
							if value, ok := alternative["speaker"].(string); ok {
								speaker = value
							}
						}
					}
				}
			}
			if err := s.event(st, "transcript", &edgeprotocol.Transcript{ID: id, Text: text, Speaker: speaker, Start: start, End: end}); err != nil {
				return
			}
			msg["edge_segment_id"] = "edge:" + strconv.FormatInt(generation, 10) + ":" + id
			msg["edge_saved"] = false
		}
		if err := st.send(msg); err != nil {
			return
		}
		if msg["message"] == "EndOfTranscript" {
			return
		}
	}
}
func (s *Server) controlStream(ctx context.Context, st *stream) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-st.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.event(st, "usage", nil); err != nil {
				st.stop()
				return
			}
			st.mu.Lock()
			expired := time.Now().After(st.grant.ExpiresAt.Add(-2 * time.Second))
			id, gen, origin := st.grant.SessionID, st.grant.Generation, st.grant.Origin
			st.mu.Unlock()
			if expired {
				st.stop()
				return
			}
			var authorization edgeprotocol.Authorization
			if s.main.Call(ctx, "renew", map[string]any{"session_id": id, "generation": gen}, &authorization) == nil {
				grant, err := edgeprotocol.Verify(s.key, authorization.Token, s.config.NodeID, origin)
				if err == nil && grant.Generation == gen && grant.SessionID == id {
					st.mu.Lock()
					st.grant = *grant
					st.mu.Unlock()
				}
			}
			st.write.Lock()
			_ = st.client.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			st.write.Unlock()
		}
	}
}

// Run retries durable events with bounded backoff and leaves rejected generations for reconciliation.
func (s *Server) Run(ctx context.Context) {
	go s.heartbeats(ctx)
	delay := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		event, err := s.queue.Next()
		if err == nil && event != nil {
			var ack edgeprotocol.Ack
			err = s.main.Call(ctx, "events", event, &ack)
			if err == nil && ack.Saved && ack.Sequence >= event.Sequence {
				err = s.queue.Ack(event, ack.Sequence)
				if err == nil {
					s.mu.Lock()
					st := s.connections[event.SessionID]
					s.mu.Unlock()
					if st != nil {
						_ = st.send(map[string]any{"message": "EdgeSaved", "generation": event.Generation, "sequence": ack.Sequence, "audio_sequence": ack.AudioSequence})
					}
					delay = time.Second
					continue
				}
			}
			if errors.Is(err, StatusError(409)) {
				_ = s.queue.Block(event)
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err != nil {
			delay = min(delay*2, 30*time.Second)
		} else {
			delay = time.Second
		}
	}
}
func (s *Server) heartbeats(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		connections := len(s.connections)
		s.mu.Unlock()
		bytes, oldest := s.queue.Stats()
		// Provider handshake verifies the independent credential without sending audio.
		if !s.providerHealthy.Load() {
			dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
			start := time.Now()
			c, response, err := dialer.DialContext(ctx, s.config.ProviderURL, http.Header{"Authorization": []string{"Bearer " + s.config.ProviderKey}})
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if err == nil {
				_ = c.Close()
				s.providerHealthy.Store(true)
				s.providerLatency.Store(time.Since(start).Milliseconds())
			}
		}
		h := edgeprotocol.Heartbeat{InstanceID: s.config.Version, Role: deployment.Default.Status().Mode, Version: s.config.Version, ProtocolMin: 1, ProtocolMax: 1, Connections: connections, ProviderLatencyMS: float64(s.providerLatency.Load()), Healthy: s.providerHealthy.Load(), Load: float64(connections) / float64(s.config.Maximum), QueueBytes: bytes, OldestEventSeconds: oldest, Successes: s.successes.Load(), Failures: s.failures.Load()}
		var response struct {
			Mode string `json:"mode"`
		}
		err := s.main.Call(ctx, "heartbeat", h, &response)
		s.enabled.Store(err == nil && response.Mode == "enabled")
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Diagnostic reports identities and counts, never node or provider secrets.
func (s *Server) Diagnostic() string {
	return fmt.Sprintf("node=%s version=%s pending=%d origins=%s", s.config.NodeID, s.config.Version, s.queue.Pending(), strings.Join(s.config.Origins, ","))
}
