package edgeruntime

import (
	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
	"testing"
)

func TestOutboxSurvivesCrashReplayAndFencesOwnership(t *testing.T) {
	dir := t.TempDir()
	q, err := OpenQueue(dir, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenQueue(dir, 1024*1024); err == nil {
		_ = other.Close()
		t.Fatal("two processes can own spool")
	}
	event := edgeprotocol.Event{SessionID: uuid.NewString(), Generation: 1, EventID: uuid.NewString(), Kind: "usage", Samples: 160, ProviderSamples: 160, AudioSequence: 1}
	if err := q.Append(&event); err != nil {
		t.Fatal(err)
	}
	if event.Sequence != 1 {
		t.Fatal(event.Sequence)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = OpenQueue(dir, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	read, err := q.Next()
	if err != nil || read == nil || read.EventID != event.EventID {
		t.Fatalf("replay lost: %+v %v", read, err)
	}
	if err := q.Ack(read, 0); err != nil {
		t.Fatal(err)
	}
	if q.Pending() != 1 {
		t.Fatal("unacknowledged event deleted")
	}
	if err := q.Block(read); err != nil {
		t.Fatal(err)
	}
	if q.Pending() != 1 {
		t.Fatal("fenced event must remain for reconciliation")
	}
	if next, err := q.Next(); err != nil || next != nil {
		t.Fatal("blocked generation still retried")
	}
	if err := q.Ack(read, 1); err != nil {
		t.Fatal(err)
	}
	if q.Pending() != 0 {
		t.Fatal("durable ack not reclaimed")
	}
}
