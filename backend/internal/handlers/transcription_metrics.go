package handlers

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

type audioTimeAnchor struct {
	end float64
	at  time.Time
}

// Keep a bounded timeline of actually forwarded audio. Pauses do not inflate
// model latency, and buffered uploads are measured from receipt upstream.
func (m *audioUsageMeter) noteAudioForwarded(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bytesPerSecond == 0 {
		return
	}
	anchor := audioTimeAnchor{end: float64(m.totalBytes) / float64(m.bytesPerSecond), at: at}
	if len(m.timeline) > 0 && anchor.at.Sub(m.timeline[len(m.timeline)-1].at) < 100*time.Millisecond {
		m.timeline[len(m.timeline)-1] = anchor
		return
	}
	if len(m.timeline) >= 8192 {
		m.timelineFloor = m.timeline[4095].end
		copy(m.timeline, m.timeline[4096:])
		m.timeline = m.timeline[:4096]
	}
	m.timeline = append(m.timeline, anchor)
}
func (m *audioUsageMeter) noteTranscript(data []byte, at time.Time) {
	var event struct {
		Message  string `json:"message"`
		Metadata struct {
			End        float64 `json:"end_time"`
			Transcript string  `json:"transcript"`
		} `json:"metadata"`
	}
	if json.Unmarshal(data, &event) != nil || event.Message != "AddTranscript" || event.Metadata.End <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(event.Metadata.Transcript) != "" {
		m.finalTranscript = true
	}
	if len(m.timeline) == 0 || event.Metadata.End <= m.lastMetricEnd || event.Metadata.End <= m.timelineFloor {
		return
	}
	i := sort.Search(len(m.timeline), func(i int) bool { return m.timeline[i].end >= event.Metadata.End })
	if i == len(m.timeline) {
		return
	}
	latency := float64(at.Sub(m.timeline[i].at)) / float64(time.Millisecond)
	if math.IsNaN(latency) || math.IsInf(latency, 0) || latency < 0 {
		return
	}
	m.lastMetricEnd = event.Metadata.End
	// Retain the most recent 8192 samples for unusually long streams.
	m.metricCount++
	if len(m.latencies) < 8192 {
		m.latencies = append(m.latencies, latency)
	} else {
		m.latencies[(m.metricCount-1)%8192] = latency
	}
}
func (h *SpeechmaticsProxyHandler) saveTranscriptionMetrics(m *audioUsageMeter, connID, userID string, sessionID *string) {
	recorder, ok := h.billing.(interface {
		RecordSessionMetrics(context.Context, string, string, *string, float64, float64, int, string) error
	})
	if !ok || userID == "" {
		return
	}
	m.mu.Lock()
	values := append([]float64(nil), m.latencies...)
	route := "unknown"
	if m.route != nil {
		route = "standard"
		if m.route.Training {
			route = "training"
		}
	}
	m.mu.Unlock()
	if len(values) == 0 {
		return
	}
	sort.Float64s(values)
	percentile := func(p float64) float64 { return values[int(math.Ceil(float64(len(values))*p))-1] }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Metrics failure must never prevent financial settlement or disconnect.
	_ = recorder.RecordSessionMetrics(ctx, connID, userID, sessionID, percentile(.5), percentile(.9), len(values), route)
}
