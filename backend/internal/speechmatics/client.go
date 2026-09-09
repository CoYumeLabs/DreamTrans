package speechmatics

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/gorilla/websocket"
)

const (
	realtimeAPIURL = "wss://global.rt.speechmatics.com/v2"
	// Message types from Speechmatics
	msgRecognitionStarted   = "RecognitionStarted"
	msgAddTranscript        = "AddTranscript"
	msgAddPartialTranscript = "AddPartialTranscript"
	msgEndOfTranscript      = "EndOfTranscript"
	msgAudioAdded           = "AudioAdded"
	msgError                = "Error"
	msgWarning              = "Warning"
	msgInfo                 = "Info"
	maxRealtimeMessageSize  = 1 << 20
	maxRealtimeAudioChunk   = 1 << 20
	realtimeReadWait        = 60 * time.Second
)

// Client handles real-time streaming transcription with Speechmatics
type Client struct {
	apiKey         string
	tokenGenerator *auth.TokenGenerator
}

type serializedWriter struct {
	conn        *websocket.Conn
	mu          sync.Mutex
	eosOnce     sync.Once
	eosErr      error
	audioChunks int64
}

func (w *serializedWriter) endOfStream() error {
	w.eosOnce.Do(func() {
		// The realtime API schema requires last_seq_no: the number of binary
		// AddAudio messages sent on this connection.
		w.eosErr = w.writeJSON(map[string]interface{}{
			"message":     "EndOfStream",
			"last_seq_no": w.sentAudioChunks(),
		})
	})
	return w.eosErr
}

func (w *serializedWriter) sentAudioChunks() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.audioChunks
}

func (w *serializedWriter) writeJSON(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return w.conn.WriteJSON(value)
}

func (w *serializedWriter) writeMessage(messageType int, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if err := w.conn.WriteMessage(messageType, payload); err != nil {
		return err
	}
	if messageType == websocket.BinaryMessage {
		w.audioChunks++
	}
	return nil
}

// NewClient creates a new Speechmatics real-time client for server-to-server
// integrations (PCAS). Those calls carry no user who could have joined the
// training program, so they use the no-training account whenever one is
// configured and fall back to SM_API_KEY otherwise.
func NewClient() (*Client, error) {
	apiKey := strings.TrimSpace(os.Getenv("SM_API_KEY_NO_TRAINING"))
	if apiKey == "" {
		apiKey = os.Getenv("SM_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("SM_API_KEY environment variable not set")
	}

	tokenGen, err := auth.NewTokenGenerator()
	if err != nil {
		return nil, fmt.Errorf("failed to create token generator: %w", err)
	}

	return &Client{
		apiKey:         apiKey,
		tokenGenerator: tokenGen,
	}, nil
}

// StreamingConfig contains configuration for the streaming transcription
type StreamingConfig struct {
	Language       string
	EnablePartials bool
	MaxDelay       float64
}

// StartStreamingTranscription starts a streaming transcription session
//
//nolint:gocyclo // Connection setup and coordinated goroutine shutdown form one lifecycle.
func (c *Client) StartStreamingTranscription(ctx context.Context, config StreamingConfig, audioInput <-chan []byte, textOutput chan<- string) error {
	if ctx == nil {
		return fmt.Errorf("context must not be nil")
	}
	if audioInput == nil || textOutput == nil {
		return fmt.Errorf("audio input and text output channels must not be nil")
	}
	config, err := normalizeStreamingConfig(config)
	if err != nil {
		return err
	}

	// Generate temporary JWT token
	token, err := c.tokenGenerator.GenerateTokenContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to generate token: %w", err)
	}

	// Build WebSocket URL with JWT
	wsURL, err := url.Parse(realtimeAPIURL)
	if err != nil {
		return fmt.Errorf("failed to parse WebSocket URL: %w", err)
	}
	q := wsURL.Query()
	q.Set("jwt", token)
	wsURL.RawQuery = q.Encode()

	// Connect to WebSocket
	log.Printf("Connecting to Speechmatics WebSocket at %s%s", wsURL.Host, wsURL.Path)
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, handshakeResponse, err := dialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		if handshakeResponse != nil && handshakeResponse.Body != nil {
			_ = handshakeResponse.Body.Close()
		}
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}
	conn.SetReadLimit(maxRealtimeMessageSize)
	if err := conn.SetReadDeadline(time.Now().Add(realtimeReadWait)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to set WebSocket read deadline: %w", err)
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(realtimeReadWait))
	})
	writer := &serializedWriter{conn: conn}
	streamCtx, cancel := context.WithCancel(ctx)
	var goroutineWG sync.WaitGroup
	defer func() {
		cancel()
		// Gorilla permits Close concurrently with readers and writers; this
		// wakes both goroutines before we wait for them.
		_ = conn.Close()
		goroutineWG.Wait()
	}()

	// Send StartRecognition message
	startMsg := map[string]interface{}{
		"message": "StartRecognition",
		"audio_format": map[string]interface{}{
			"type":        "raw",
			"encoding":    "pcm_f32le",
			"sample_rate": 48000,
		},
		"transcription_config": map[string]interface{}{
			"language":                 config.Language,
			"enable_partials":          config.EnablePartials,
			"operating_point":          "enhanced",
			"enable_entities":          true,
			"speaker_diarization":      "speaker",
			"diarization_max_speakers": 10,
		},
	}

	if config.MaxDelay > 0 {
		startMsg["transcription_config"].(map[string]interface{})["max_delay"] = config.MaxDelay
	}

	if err := writer.writeJSON(startMsg); err != nil {
		return fmt.Errorf("failed to send StartRecognition: %w", err)
	}

	// Create error channel for goroutines
	errChan := make(chan error, 2)

	// Start goroutine to read messages from WebSocket
	goroutineWG.Add(1)
	go func() {
		defer goroutineWG.Done()
		c.readMessages(streamCtx, conn, textOutput, errChan)
	}()

	// Start goroutine to send audio data
	goroutineWG.Add(1)
	go func() {
		defer goroutineWG.Done()
		c.sendAudio(streamCtx, writer, audioInput, errChan)
	}()

	// Wait for context cancellation or error
	select {
	case <-ctx.Done():
		log.Println("Streaming transcription context canceled")
		// Send EndOfStream message
		if err := writer.endOfStream(); err != nil {
			log.Printf("Failed to send EndOfStream: %v", err)
		}
		return ctx.Err()
	case err := <-errChan:
		return err
	}
}

func normalizeStreamingConfig(config StreamingConfig) (StreamingConfig, error) {
	config.Language = strings.TrimSpace(config.Language)
	if config.Language == "" {
		config.Language = "en"
	}
	if !validProtocolValue(config.Language, 10, false) {
		return StreamingConfig{}, fmt.Errorf("invalid streaming language")
	}
	if math.IsNaN(config.MaxDelay) || math.IsInf(config.MaxDelay, 0) ||
		config.MaxDelay < 0 || config.MaxDelay > 30 {
		return StreamingConfig{}, fmt.Errorf("invalid streaming max delay")
	}
	return config, nil
}

// readMessages reads messages from the WebSocket and processes them
// nolint:gocyclo
func (c *Client) readMessages(ctx context.Context, conn *websocket.Conn, textOutput chan<- string, errChan chan<- error) {
	defer close(textOutput)
	// Normal peer closure must wake StartStreamingTranscription as well. Older
	// code returned silently and could leave the caller blocked forever.
	defer reportStreamingResult(errChan, nil)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// Read message with timeout
			if err := conn.SetReadDeadline(time.Now().Add(realtimeReadWait)); err != nil {
				reportStreamingResult(errChan, fmt.Errorf("failed to set WebSocket read deadline: %w", err))
				return
			}
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					reportStreamingResult(errChan, fmt.Errorf("WebSocket read error: %w", err))
				}
				return
			}

			// Skip binary messages (server doesn't send binary to client)
			if messageType == websocket.BinaryMessage {
				continue
			}

			// Parse text message as JSON
			var msg map[string]interface{}
			if err := json.Unmarshal(message, &msg); err != nil {
				log.Printf("Failed to parse message: %v", err)
				continue
			}

			// Handle different message types
			msgType, ok := msg["message"].(string)
			if !ok {
				continue
			}

			switch msgType {
			case msgRecognitionStarted:
				log.Println("Recognition started")

			case msgAddTranscript:
				// Extract final transcript
				if metadata, ok := msg["metadata"].(map[string]interface{}); ok {
					if transcript, ok := metadata["transcript"].(string); ok && transcript != "" {
						select {
						case textOutput <- transcript:
						case <-ctx.Done():
							return
						}
					}
				}

			case msgAddPartialTranscript:
				// Extract partial transcript (optional - could be filtered out)
				if metadata, ok := msg["metadata"].(map[string]interface{}); ok {
					if transcript, ok := metadata["transcript"].(string); ok && transcript != "" {
						// Prefix with [PARTIAL] to distinguish from final transcripts
						select {
						case textOutput <- "[PARTIAL] " + transcript:
						case <-ctx.Done():
							return
						}
					}
				}

			case msgEndOfTranscript:
				log.Println("End of transcript received")
				return

			case msgError:
				errorMsg := "Speechmatics error: " + speechmaticsMessageDetail(msg)
				log.Println(errorMsg)
				reportStreamingResult(errChan, fmt.Errorf("%s", errorMsg))
				return

			case msgWarning:
				log.Printf("Speechmatics warning: %s", speechmaticsMessageDetail(msg))

			case msgInfo:
				log.Printf("Speechmatics info: %s", speechmaticsMessageDetail(msg))

			case msgAudioAdded:
				// Audio successfully added, no action needed
			}
		}
	}
}

// sendAudio sends audio data to the WebSocket
func (c *Client) sendAudio(ctx context.Context, writer *serializedWriter, audioInput <-chan []byte, errChan chan<- error) {
	for {
		select {
		case <-ctx.Done():
			return
		case audioData, ok := <-audioInput:
			if !ok {
				// Audio input channel closed, send EndOfStream
				if err := writer.endOfStream(); err != nil {
					reportStreamingResult(errChan, fmt.Errorf("failed to send EndOfStream: %w", err))
				}
				return
			}
			if len(audioData) == 0 {
				continue
			}
			if len(audioData) > maxRealtimeAudioChunk {
				reportStreamingResult(
					errChan,
					fmt.Errorf("audio chunk exceeds %d bytes", maxRealtimeAudioChunk),
				)
				return
			}

			// Send audio data as binary message directly
			if err := writer.writeMessage(websocket.BinaryMessage, audioData); err != nil {
				reportStreamingResult(errChan, fmt.Errorf("failed to send audio: %w", err))
				return
			}
		}
	}
}

func speechmaticsMessageDetail(message map[string]interface{}) string {
	for _, key := range []string{"reason", "error", "code", "type"} {
		value, ok := message[key].(string)
		if !ok {
			continue
		}
		value = strings.Map(func(character rune) rune {
			if character < 0x20 || character == 0x7f {
				return ' '
			}
			return character
		}, value)
		value = strings.TrimSpace(value)
		runes := []rune(value)
		if len(runes) > 256 {
			value = string(runes[:256]) + "..."
		}
		if value != "" {
			return value
		}
	}
	return "upstream message did not include a reason"
}

func reportStreamingResult(errChan chan<- error, err error) {
	select {
	case errChan <- err:
	default:
	}
}
