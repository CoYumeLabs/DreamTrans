// Package edgeprotocol defines the bounded, versioned main-site/Edge contract.
package edgeprotocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	Version       = 2
	MinVersion    = 1
	LeaseSeconds  = 45
	BudgetSeconds = 30
	MaxEventBytes = 256 * 1024
	MaxAudioBytes = 64 * 1024
)

// Grant never contains a main-site bearer credential or signing private key.
type Grant struct {
	DurableAudioSequence  int64   `json:"durable_audio_sequence,omitempty"`
	DurableSamples        int64   `json:"durable_samples,omitempty"`
	ResumeSamples         int64   `json:"resume_samples"`
	TimelineOffset        float64 `json:"timeline_offset"`
	PreviousGeneration    int64   `json:"previous_generation"`
	PreviousAudioSequence int64   `json:"previous_audio_sequence"`
	jwt.RegisteredClaims
	NodeID          string `json:"node_id"`
	SessionID       string `json:"session_id"`
	UserID          string `json:"user_id"`
	Generation      int64  `json:"generation"`
	Provider        string `json:"provider"`
	Origin          string `json:"origin"`
	Training        bool   `json:"training"`
	SampleRate      int    `json:"sample_rate"`
	ApprovedSamples int64  `json:"approved_samples"`
	AudioSequence   int64  `json:"audio_sequence"`
	Protocol        int    `json:"protocol"`
}

// Authorization fixes a session to one endpoint until an explicit generation change.
type Authorization struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	Grant    Grant  `json:"grant"`
}

// Event is persisted on Edge before submission. Samples and audio seq are cumulative.
type Event struct {
	DurableAudioSequence int64       `json:"durable_audio_sequence,omitempty"`
	DurableSamples       int64       `json:"durable_samples,omitempty"`
	SessionID            string      `json:"session_id"`
	Generation           int64       `json:"generation"`
	Sequence             int64       `json:"sequence"`
	EventID              string      `json:"event_id"`
	Kind                 string      `json:"kind"`
	AudioSequence        int64       `json:"audio_sequence"`
	Samples              int64       `json:"samples"`
	ProviderSamples      int64       `json:"provider_samples"`
	Transcript           *Transcript `json:"transcript,omitempty"`
}

type Transcript struct {
	ID      string  `json:"id"`
	Speaker string  `json:"speaker"`
	Text    string  `json:"text"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
}

type Ack struct {
	Sequence      int64 `json:"sequence"`
	AudioSequence int64 `json:"audio_sequence"`
	Saved         bool  `json:"saved"`
}

type Heartbeat struct {
	InstanceID         string  `json:"instance_id"`
	Role               string  `json:"role"`
	Version            string  `json:"version"`
	ProtocolMin        int     `json:"protocol_min"`
	ProtocolMax        int     `json:"protocol_max"`
	Connections        int     `json:"connections"`
	ProviderLatencyMS  float64 `json:"provider_latency_ms"`
	Healthy            bool    `json:"healthy"`
	Load               float64 `json:"load"`
	QueueBytes         int64   `json:"queue_bytes"`
	OldestEventSeconds float64 `json:"oldest_event_seconds"`
	Successes          int64   `json:"successes"`
	Failures           int64   `json:"failures"`
	CaptionP50MS       float64 `json:"caption_p50_ms"`
	CaptionP95MS       float64 `json:"caption_p95_ms"`
}

func Secret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func Hash(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
func Sign(key ed25519.PrivateKey, grant *Grant) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, grant).SignedString(key)
}
func Verify(key ed25519.PublicKey, token, node, origin string) (*Grant, error) {
	var grant Grant
	_, err := jwt.ParseWithClaims(token, &grant, func(t *jwt.Token) (any, error) { return key, nil }, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithExpirationRequired(), jwt.WithIssuer("dreamtrans-edge"), jwt.WithAudience(node), jwt.WithLeeway(2*time.Second))
	if err != nil {
		return nil, err
	}
	if grant.NodeID != node || grant.Origin != origin || (grant.Protocol < MinVersion || grant.Protocol > Version) || grant.Provider != "speechmatics" || grant.ID == "" || grant.SessionID == "" || grant.Generation < 1 || !ValidSampleRate(grant.SampleRate) || grant.ApprovedSamples < 0 {
		return nil, errors.New("invalid edge grant")
	}
	return &grant, nil
}

// ValidSampleRate accepts the PCM clocks used by browser audio contexts.
func ValidSampleRate(rate int) bool { return rate == 16000 || rate == 44100 || rate == 48000 }
