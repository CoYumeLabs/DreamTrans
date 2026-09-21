package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
)

const (
	speechmaticsRealtimeURL = "wss://global.rt.speechmatics.com/v2"

	// WebSocket connection parameters for robustness
	writeWait      = 10 * time.Second // Time allowed to write a message
	pongWait       = 60 * time.Second // Time allowed to read the next pong message
	pingPeriod     = 30 * time.Second // Send pings with this period (must be less than pongWait)
	maxMessageSize = 64 * 1024        // Maximum message size (64KB for audio chunks)

	// Reserve a small rolling window before forwarding audio upstream. The
	// unused tail is settled back to the exact forwarded byte count when the
	// connection ends, so short sessions are not rounded up to this interval.
	speechmaticsReservationPeriod = 5 * time.Second
)

type audioUsageReservation struct {
	key           string
	startBytes    uint64
	reservedBytes uint64
	minutes       float64
	confirmed     bool
}

type audioUsageSettlement struct {
	translation bool
	key         string
	minutes     float64
}

type audioUsageSnapshot struct {
	totalBytes uint64
	minutes    float64
}

// audioUsageMeter derives billable duration from forwarded raw audio bytes.
// Wall-clock connection time is not a useful proxy because clients can pause,
// buffer, or send audio faster/slower than real time.
type audioUsageMeter struct {
	mu              sync.Mutex
	timeline        []audioTimeAnchor
	latencies       []float64
	metricCount     int
	finalTranscript bool
	translation     bool
	lastMetricEnd   float64
	timelineFloor   float64
	// route is the session's routing decision, fixed at connect so every
	// reservation is priced alike; nil lets the ledger decide per record.
	route          *billing.RouteDecision
	configured     bool
	bytesPerSecond uint64
	totalBytes     uint64
	chargedBytes   uint64
	reservedBytes  uint64
	reservations   []audioUsageReservation
}

func (m *audioUsageMeter) ConfigureStartRecognition(data []byte) (bool, error) {
	var envelope struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil ||
		!strings.EqualFold(envelope.Message, "StartRecognition") {
		return false, nil
	}

	var request struct {
		Translation *struct {
			TargetLanguages []string `json:"target_languages"`
		} `json:"translation_config"`
		AudioFormat struct {
			Type         string `json:"type"`
			Encoding     string `json:"encoding"`
			SampleRate   int    `json:"sample_rate"`
			Channels     int    `json:"channels"`
			ChannelCount int    `json:"channel_count"`
		} `json:"audio_format"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return true, fmt.Errorf("invalid StartRecognition message: %w", err)
	}
	if request.Translation != nil {
		if len(request.Translation.TargetLanguages) != 1 {
			return true, fmt.Errorf("exactly one translation target is supported")
		}
		language := request.Translation.TargetLanguages[0]
		if len(language) < 2 || len(language) > 12 {
			return true, fmt.Errorf("invalid translation target")
		}
		for _, ch := range language {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && ch != '-' {
				return true, fmt.Errorf("invalid translation target")
			}
		}
	}
	format := request.AudioFormat
	if !strings.EqualFold(strings.TrimSpace(format.Type), "raw") {
		return true, fmt.Errorf("billing requires raw audio format")
	}
	if format.SampleRate < 8000 || format.SampleRate > 192000 {
		return true, fmt.Errorf("sample_rate must be between 8000 and 192000")
	}
	channels := format.Channels
	if channels == 0 {
		channels = format.ChannelCount
	}
	if channels == 0 {
		channels = 1
	}
	if channels < 1 || channels > 8 {
		return true, fmt.Errorf("channels must be between 1 and 8")
	}
	bytesPerSample, ok := rawAudioBytesPerSample(format.Encoding)
	if !ok {
		return true, fmt.Errorf("unsupported raw audio encoding")
	}
	// Values are range-checked above, so each conversion and the product fit.
	bytesPerSecond := uint64(format.SampleRate) * uint64(channels) * uint64(bytesPerSample) // #nosec G115

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configured && (m.totalBytes > 0 || m.reservedBytes > 0) &&
		m.bytesPerSecond != bytesPerSecond {
		return true, fmt.Errorf("audio format cannot change after audio has started")
	}
	if m.configured && m.translation != (request.Translation != nil) {
		return true, fmt.Errorf("translation cannot change after recognition starts")
	}
	m.translation = request.Translation != nil
	m.configured = true
	m.bytesPerSecond = bytesPerSecond
	return true, nil
}

func rawAudioBytesPerSample(encoding string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "pcm_s8", "pcm_u8", "mulaw", "alaw":
		return 1, true
	case "pcm_s16le", "pcm_s16be":
		return 2, true
	case "pcm_s24le", "pcm_s24be":
		return 3, true
	case "pcm_s32le", "pcm_s32be", "pcm_f32le", "pcm_f32be":
		return 4, true
	case "pcm_f64le", "pcm_f64be":
		return 8, true
	default:
		return 0, false
	}
}

func (m *audioUsageMeter) AddForwardedBytes(count int) error {
	if count <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.bytesPerSecond == 0 {
		return fmt.Errorf("start recognition with a supported raw audio format is required before audio")
	}
	increment := uint64(count)
	if ^uint64(0)-m.totalBytes < increment {
		return fmt.Errorf("audio byte counter overflow")
	}
	m.totalBytes += increment
	return nil
}

// AddReservedForwardedBytes commits audio bytes only after a successful usage
// reservation. Calling it without enough prepaid coverage is always rejected.
func (m *audioUsageMeter) AddReservedForwardedBytes(count int) error {
	if count <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.bytesPerSecond == 0 {
		return fmt.Errorf("start recognition with a supported raw audio format is required before audio")
	}
	increment := uint64(count)
	if ^uint64(0)-m.totalBytes < increment {
		return fmt.Errorf("audio byte counter overflow")
	}
	nextTotal := m.totalBytes + increment
	if nextTotal > m.reservedBytes {
		return fmt.Errorf("audio usage has not been reserved")
	}
	m.totalBytes = nextTotal
	return nil
}

func (m *audioUsageMeter) AudioReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configured && m.bytesPerSecond > 0
}

// Pending and Commit remain useful to callers which account for already
// forwarded audio without rolling reservations.
func (m *audioUsageMeter) Pending() (audioUsageSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.bytesPerSecond == 0 || m.totalBytes <= m.chargedBytes {
		return audioUsageSnapshot{}, false
	}
	pendingBytes := m.totalBytes - m.chargedBytes
	return audioUsageSnapshot{
		totalBytes: m.totalBytes,
		minutes:    float64(pendingBytes) / float64(m.bytesPerSecond) / 60,
	}, true
}

func (m *audioUsageMeter) Commit(snapshot audioUsageSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if snapshot.totalBytes > m.chargedBytes && snapshot.totalBytes <= m.totalBytes {
		m.chargedBytes = snapshot.totalBytes
	}
}

// ReserveNextBytes allocates prepaid coverage for the next audio frame. It is
// called before the frame is written upstream. The returned reservation must
// be charged successfully before AddReservedForwardedBytes is called.
func (m *audioUsageMeter) ReserveNextBytes(
	count int,
	connectionID string,
) (*audioUsageReservation, error) {
	if count <= 0 {
		return nil, nil
	}
	if strings.TrimSpace(connectionID) == "" {
		return nil, fmt.Errorf("billing connection id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.bytesPerSecond == 0 {
		return nil, fmt.Errorf("start recognition with a supported raw audio format is required before audio")
	}
	increment := uint64(count)
	if ^uint64(0)-m.totalBytes < increment {
		return nil, fmt.Errorf("audio byte counter overflow")
	}
	requiredBytes := m.totalBytes + increment
	if requiredBytes <= m.reservedBytes {
		return nil, nil
	}

	quantumBytes := m.bytesPerSecond * uint64(speechmaticsReservationPeriod/time.Second)
	reservationBytes := requiredBytes - m.reservedBytes
	if reservationBytes < quantumBytes {
		reservationBytes = quantumBytes
	}
	if ^uint64(0)-m.reservedBytes < reservationBytes {
		return nil, fmt.Errorf("audio reservation counter overflow")
	}
	reservation := audioUsageReservation{
		key:           fmt.Sprintf("speechmatics:%s:reserve:%d", connectionID, len(m.reservations)+1),
		startBytes:    m.reservedBytes,
		reservedBytes: reservationBytes,
		minutes:       float64(reservationBytes) / float64(m.bytesPerSecond) / 60,
	}
	m.reservedBytes += reservationBytes
	m.reservations = append(m.reservations, reservation)
	copyOfReservation := reservation
	return &copyOfReservation, nil
}

func (m *audioUsageMeter) ConfirmReservation(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for index := range m.reservations {
		if m.reservations[index].key == key {
			m.reservations[index].confirmed = true
			return
		}
	}
}

// PendingSettlements returns only reservation tails which differ from their
// actual forwarded audio. Confirmed reservations are refunded down to exact
// usage; an unconfirmed reservation is also reconciled to zero in case its
// database commit succeeded but the caller observed an ambiguous error.
func (m *audioUsageMeter) PendingSettlements() []audioUsageSettlement {
	m.mu.Lock()
	defer m.mu.Unlock()
	settlements := make([]audioUsageSettlement, 0, 1)
	for _, reservation := range m.reservations {
		var actualBytes uint64
		if reservation.confirmed && m.totalBytes > reservation.startBytes {
			actualBytes = m.totalBytes - reservation.startBytes
			if actualBytes > reservation.reservedBytes {
				actualBytes = reservation.reservedBytes
			}
		}
		if actualBytes == reservation.reservedBytes {
			continue
		}
		settlements = append(settlements, audioUsageSettlement{
			key:     reservation.key,
			minutes: float64(actualBytes) / float64(m.bytesPerSecond) / 60,
		})
	}
	if m.translation {
		originals := append([]audioUsageSettlement(nil), settlements...)
		for _, item := range originals {
			item.key += ":translation"
			item.translation = true
			settlements = append(settlements, item)
		}
	}
	return settlements
}
