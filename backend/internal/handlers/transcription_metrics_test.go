package handlers

import (
	"testing"
	"time"
)

func TestTranscriptionLatencyExcludesPauseAndDeduplicatesFinals(t *testing.T) {
	start := time.Now()
	meter := &audioUsageMeter{bytesPerSecond: 32000, totalBytes: 32000}
	meter.noteAudioForwarded(start)
	meter.noteTranscript([]byte(`{"message":"AddTranscript","metadata":{"end_time":1}}`), start.Add(250*time.Millisecond))
	meter.totalBytes = 64000
	meter.noteAudioForwarded(start.Add(5 * time.Minute))
	meter.noteTranscript([]byte(`{"message":"AddTranscript","metadata":{"end_time":2}}`), start.Add(5*time.Minute+400*time.Millisecond))
	meter.noteTranscript([]byte(`{"message":"AddTranscript","metadata":{"end_time":2}}`), start.Add(6*time.Minute))
	meter.noteTranscript([]byte(`{"message":"AddPartialTranscript","metadata":{"end_time":3}}`), start.Add(6*time.Minute))
	if len(meter.latencies) != 2 || meter.latencies[0] != 250 || meter.latencies[1] != 400 {
		t.Fatalf("latencies=%v", meter.latencies)
	}
}
func TestAuditPayloadRedactsNestedCredentials(t *testing.T) {
	result := redactAuditPayload(map[string]any{"settings": map[string]any{"api_key": "secret", "password": "private", "amount_usd": 5}}).(map[string]any)
	nested := result["settings"].(map[string]any)
	if nested["api_key"] != "[redacted]" || nested["password"] != "[redacted]" || nested["amount_usd"] != 5 {
		t.Fatalf("redaction=%v", result)
	}
}
