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
	"net"
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
type audioBoundary struct{ sequence, sampleEnd int64 }

// streamExit contains only bounded classifications, never provider error text,
// credentials, URLs, or transcript/audio contents.
type streamExit struct {
	Reason    string
	CloseCode int
	Timeout   bool
}
type stream struct {
	exit                               atomic.Pointer[streamExit]
	providerPending                    []audioBoundary
	providerAckSamples                 int64
	providerProgress                   chan struct{}
	durableSeq, durableSamples         int64
	audioBounds                        []audioBoundary
	providerFrames                     int64
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

// recordExit preserves the initiating failure rather than the socket-close
// errors caused by canceling the other half of the connection.
func (st *stream) recordExit(reason string, err error) {
	detail := &streamExit{Reason: reason}
	var closeError *websocket.CloseError
	if errors.As(err, &closeError) {
		detail.CloseCode = closeError.Code
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		detail.Timeout = networkError.Timeout()
	}
	st.exit.CompareAndSwap(nil, detail)
}
func (s *Server) logExit(st *stream) {
	st.recordExit("handler_exit", nil)
	detail := st.exit.Load()
	st.mu.Lock()
	id, generation, samples, sequence := st.grant.SessionID, st.grant.Generation, st.samples, st.audioSeq
	st.mu.Unlock()
	log.Printf("edge session=%s generation=%d exit=%s close_code=%d timeout=%t samples=%d audio_sequence=%d duration_ms=%d", id, generation, detail.Reason, detail.CloseCode, detail.Timeout, samples, sequence, time.Since(st.started).Milliseconds())
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
	st := &stream{client: client, grant: *grant, done: make(chan struct{}), providerProgress: make(chan struct{}, 1), started: time.Now(), audioSeq: grant.DurableAudioSequence, durableSeq: grant.DurableAudioSequence, durableSamples: grant.DurableSamples}
	defer s.logExit(st)
	s.mu.Lock()
	if len(s.connections) >= s.config.Maximum || s.connections[grant.SessionID] != nil {
		s.mu.Unlock()
		st.recordExit("edge_capacity_or_duplicate", nil)
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
		st.recordExit("main_connect_failed", err)
		return
	}
	// Verify the main response cryptographically too; a grant cannot grow in a JSON-only response.
	authoritative, err := edgeprotocol.Verify(s.key, confirmed.Token, s.config.NodeID, r.Header.Get("Origin"))
	if err != nil {
		return
	}
	st.mu.Lock()
	st.grant = *authoritative
	st.mu.Unlock()
	start := time.Now()
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	provider, response, err := dialer.DialContext(ctx, s.config.ProviderURL, http.Header{"Authorization": []string{"Bearer " + s.config.ProviderKey}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		s.failures.Add(1)
		st.recordExit("provider_connect_failed", err)
		_ = s.event(st, "end")
		return
	}
	s.providerLatency.Store(time.Since(start).Milliseconds())
	s.successes.Add(1)
	st.provider = provider
	defer st.stop()
	var startMessage map[string]any
	if err := client.ReadJSON(&startMessage); err != nil {
		st.recordExit("client_start_read_failed", err)
		_ = s.event(st, "end")
		return
	}
	if !validateStart(startMessage, st.grant.SampleRate) {
		st.recordExit("invalid_start", nil)
		_ = s.event(st, "end")
		return
	}
	delete(startMessage, "translation_config") // phase one: translations remain on the main AI WebSocket
	if err := provider.WriteJSON(startMessage); err != nil {
		st.recordExit("provider_start_write_failed", err)
		_ = s.event(st, "end")
		return
	}
	ended := make(chan struct{})
	if st.grant.Protocol >= 2 {
		stopHandoff := deployment.Default.NotifyHandoff(st.send)
		defer stopHandoff()
	}
	go func() { defer close(ended); s.readProvider(st) }()
	controlDone := make(chan struct{})
	go func() { defer close(controlDone); s.controlStream(ctx, st) }()
	s.readAudio(st)
	cancel()
	st.stop()
	<-ended
	<-controlDone
	if err := s.event(st, "end"); err != nil {
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
			st.recordExit("client_read_failed", err)
			return
		}
		if kind == websocket.TextMessage {
			var msg map[string]any
			if json.Unmarshal(data, &msg) != nil {
				st.recordExit("invalid_client_json", nil)
				return
			}
			if msg["message"] == "EndOfStream" {
				st.mu.Lock()
				last := st.providerFrames
				st.mu.Unlock()
				_ = st.provider.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_ = st.provider.WriteJSON(map[string]any{"message": "EndOfStream", "last_seq_no": last})
				select {
				case <-st.done:
				case <-time.After(15 * time.Second):
					st.recordExit("provider_finalization_timeout", nil)
				}
				return
			}
			continue
		}
		if kind != websocket.BinaryMessage || len(data) < 12 || len(data) > edgeprotocol.MaxAudioBytes+8 || (len(data)-8)%4 != 0 {
			st.recordExit("invalid_audio_frame", nil)
			return
		}
		wireSequence := binary.BigEndian.Uint64(data[:8])
		if wireSequence > 1_000_000_000 {
			st.recordExit("invalid_audio_sequence", nil)
			return
		}
		sequence := int64(wireSequence)
		samples := int64((len(data) - 8) / 4)
		st.mu.Lock()
		if sequence <= st.audioSeq {
			st.mu.Unlock()
			continue
		}
		st.mu.Unlock()
		if !s.waitProviderCapacity(st, samples) {
			st.recordExit("provider_backpressure_or_expiry", nil)
			return
		}
		st.mu.Lock()
		if len(st.audioBounds) >= 4096 || (st.grant.Protocol >= 2 && st.grant.ResumeSamples+st.providerSamples+samples-st.durableSamples > int64(st.grant.SampleRate)*30) {
			st.mu.Unlock()
			st.recordExit("audio_replay_limit", nil)
			_ = st.send(map[string]string{"message": "Error", "type": "edge_replay_limit", "reason": "Provider finalization exceeded the bounded audio replay window"})
			return
		}
		if sequence != st.audioSeq+1 || time.Now().After(st.grant.ExpiresAt.Add(-2*time.Second)) || st.samples+samples > st.grant.ApprovedSamples {
			reason := "budget_exhausted"
			if sequence != st.audioSeq+1 {
				reason = "audio_sequence_gap"
			} else if time.Now().After(st.grant.ExpiresAt.Add(-2 * time.Second)) {
				reason = "lease_expired"
			}
			st.recordExit(reason, nil)
			st.mu.Unlock()
			_ = st.send(map[string]string{"message": "Error", "type": "edge_authorization_limit", "reason": "Edge: 已批准的额度或连接授权已到期，录音已停止；请恢复连接后继续。"})
			return
		}
		// Serialize upstream audio writes. No main-site request occurs in this loop.
		_ = st.provider.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := st.provider.WriteMessage(websocket.BinaryMessage, data[8:]); err != nil {
			st.recordExit("provider_audio_write_failed", err)
			st.mu.Unlock()
			return
		}
		st.audioSeq = sequence
		st.samples += samples
		st.providerSamples += samples
		st.providerFrames++
		st.providerPending = append(st.providerPending, audioBoundary{st.providerFrames, st.providerSamples})
		if st.grant.Protocol >= 2 {
			st.audioBounds = append(st.audioBounds, audioBoundary{sequence, st.grant.ResumeSamples + st.providerSamples})
		}
		st.mu.Unlock()
		// This only acknowledges receipt, never permanent transcript storage.
		if err := st.send(map[string]any{"message": "EdgeReceived", "sequence": sequence}); err != nil {
			st.recordExit("client_ack_write_failed", err)
			return
		}
	}
}

// Speechmatics limits unacknowledged input to 500 frames or ten seconds.
// Waiting releases the stream lock so provider reads and lease checks can progress.
func (s *Server) waitProviderCapacity(st *stream, samples int64) bool {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		st.mu.Lock()
		ready := len(st.providerPending) < 500 && st.providerSamples-st.providerAckSamples+samples <= int64(st.grant.SampleRate)*10
		expired := time.Now().After(st.grant.ExpiresAt.Add(-2 * time.Second))
		st.mu.Unlock()
		if expired {
			return false
		}
		if ready {
			return true
		}
		select {
		case <-st.done:
			return false
		case <-timer.C:
			return false
		case <-st.providerProgress:
		}
	}
}
func (st *stream) acknowledgeProvider(sequence int64) {
	st.mu.Lock()
	retired := 0
	for _, bound := range st.providerPending {
		if bound.sequence > sequence {
			break
		}
		st.providerAckSamples = bound.sampleEnd
		retired++
	}
	st.providerPending = st.providerPending[retired:]
	st.mu.Unlock()
	select {
	case st.providerProgress <- struct{}{}:
	default:
	}
}

func (s *Server) event(st *stream, kind string) error {
	return s.checkpointEvent(st, kind, nil, -1)
}

// checkpointEvent advances replay retention only with a durably queued provider
// final. A usage report or abrupt disconnect cannot discard untranscribed audio.
func (s *Server) checkpointEvent(st *stream, kind string, t *edgeprotocol.Transcript, processedSamples int64) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	durableSeq, durableSamples, retired := st.durableSeq, st.durableSamples, 0
	if processedSamples >= 0 {
		end := st.grant.ResumeSamples + min(processedSamples, st.providerSamples)
		for _, bound := range st.audioBounds {
			if bound.sampleEnd > end {
				break
			}
			durableSeq, durableSamples = bound.sequence, bound.sampleEnd
			retired++
		}
	}
	if st.grant.Protocol == 1 {
		durableSeq, durableSamples = 0, 0
	}
	e := edgeprotocol.Event{SessionID: st.grant.SessionID, Generation: st.grant.Generation, EventID: uuid.NewString(), Kind: kind, AudioSequence: st.audioSeq, Samples: st.samples, ProviderSamples: st.providerSamples, Transcript: t, DurableAudioSequence: durableSeq, DurableSamples: durableSamples}
	if err := s.queue.Append(&e); err != nil {
		return err
	}
	st.durableSeq, st.durableSamples = durableSeq, durableSamples
	st.audioBounds = st.audioBounds[retired:]
	return nil
}
func (s *Server) readProvider(st *stream) {
	st.mu.Lock()
	offset, generation, rate, protocol := st.grant.TimelineOffset, st.grant.Generation, st.grant.SampleRate, st.grant.Protocol
	st.mu.Unlock()
	defer st.stop()
	st.provider.SetReadLimit(256 * 1024)
	for {
		_, raw, err := st.provider.ReadMessage()
		if err != nil {
			st.recordExit("provider_read_failed", err)
			return
		}
		var msg map[string]any
		if json.Unmarshal(raw, &msg) != nil {
			st.recordExit("invalid_provider_json", nil)
			return
		}
		if msg["message"] == "Error" {
			st.recordExit("provider_error", nil)
		}
		if msg["message"] == "AudioAdded" {
			sequence, _ := msg["seq_no"].(float64)
			st.acknowledgeProvider(int64(sequence))
		}
		if msg["message"] == "AddPartialTranscript" {
			if metadata, ok := msg["metadata"].(map[string]any); ok {
				start, _ := metadata["start_time"].(float64)
				end, _ := metadata["end_time"].(float64)
				metadata["start_time"], metadata["end_time"] = start+offset, end+offset
				msg["edge_absolute_time"] = true
			}
		}
		if msg["message"] == "AddTranscript" {
			metadata, ok := msg["metadata"].(map[string]any)
			if !ok {
				st.recordExit("invalid_provider_transcript", nil)
				return
			}
			text, _ := metadata["transcript"].(string)
			start, _ := metadata["start_time"].(float64)
			end, _ := metadata["end_time"].(float64)
			processedSamples := int64(end * float64(rate))
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
			kind := "transcript"
			transcript := &edgeprotocol.Transcript{ID: id, Text: text, Speaker: speaker, Start: start, End: end}
			if strings.TrimSpace(text) == "" {
				if protocol == 1 {
					continue
				}
				kind, transcript = "checkpoint", nil
			}
			if err := s.checkpointEvent(st, kind, transcript, processedSamples); err != nil {
				st.recordExit("outbox_checkpoint_failed", err)
				return
			}
			msg["edge_segment_id"] = "edge:" + strconv.FormatInt(generation, 10) + ":" + id
			msg["edge_saved"] = false
		}
		if msg["message"] == "EndOfTranscript" && protocol >= 2 {
			st.mu.Lock()
			processed := st.providerSamples
			st.mu.Unlock()
			if err := s.checkpointEvent(st, "checkpoint", nil, processed); err != nil {
				st.recordExit("outbox_checkpoint_failed", err)
				return
			}
		}
		if err := st.send(msg); err != nil {
			st.recordExit("client_transcript_write_failed", err)
			return
		}
		if msg["message"] == "EndOfTranscript" {
			st.recordExit("completed", nil)
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
			if err := s.event(st, "usage"); err != nil {
				st.recordExit("outbox_usage_failed", err)
				st.stop()
				return
			}
			st.mu.Lock()
			expired := time.Now().After(st.grant.ExpiresAt.Add(-2 * time.Second))
			id, gen, origin := st.grant.SessionID, st.grant.Generation, st.grant.Origin
			st.mu.Unlock()
			if expired {
				st.recordExit("lease_expired", nil)
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
	ctx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() { defer close(heartbeatDone); s.heartbeats(ctx) }()
	defer func() { cancel(); <-heartbeatDone }()
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
		// Archive at most one quarantined event between normal delivery batches.
		// An old main (404), unknown historic ownership, or a network partition keeps
		// the record intact and backs off that generation without blocking others.
		if err == nil && event == nil {
			retired, archiveErr := s.queue.NextArchive()
			if archiveErr == nil && retired != nil {
				var ack edgeprotocol.ArchiveAck
				archiveErr = s.main.Call(ctx, "archive", retired, &ack)
				if archiveErr == nil {
					archiveErr = s.queue.Archive(retired, &ack)
				}
				if archiveErr == nil {
					s.mu.Lock()
					writer := s.connections[retired.SessionID]
					active := false
					if writer != nil {
						writer.mu.Lock()
						active = writer.grant.Generation == retired.Generation
						writer.mu.Unlock()
					}
					s.mu.Unlock()
					if !active {
						_ = s.queue.retireCounter(retired)
					}
					log.Printf("edge session=%s generation=%d archived_event=%s disposition=%s", retired.SessionID, retired.Generation, retired.EventID, ack.Disposition)
					delay = time.Second
					continue
				}
				_ = s.queue.RetryArchive(retired)
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
		h := edgeprotocol.Heartbeat{InstanceID: s.config.Version, Role: deployment.Default.Status().Mode, Version: s.config.Version, ProtocolMin: edgeprotocol.MinVersion, ProtocolMax: edgeprotocol.Version, Connections: connections, ProviderLatencyMS: float64(s.providerLatency.Load()), Healthy: s.providerHealthy.Load(), Load: float64(connections) / float64(s.config.Maximum), QueueBytes: bytes, OldestEventSeconds: oldest, Successes: s.successes.Load(), Failures: s.failures.Load()}
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
