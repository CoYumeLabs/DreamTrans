package edgeruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestArchiveReceiptIsExactAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := OpenQueue(dir, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	event := edgeprotocol.Event{SessionID: uuid.NewString(), Generation: 1, EventID: uuid.NewString(), Kind: "end"}
	if err = q.Append(&event); err != nil {
		t.Fatal(err)
	}
	if err = q.Block(&event); err != nil {
		t.Fatal(err)
	}
	if err = q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = OpenQueue(dir, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	next, err := q.NextArchive()
	if err != nil || next == nil || next.EventID != event.EventID {
		t.Fatalf("lost quarantine: %v %v", next, err)
	}
	data, _ := json.Marshal(event)
	ack := edgeprotocol.ArchiveAck{SessionID: event.SessionID, Generation: 1, Sequence: 1, EventID: event.EventID, PayloadHash: edgeprotocol.Hash(string(data)), Disposition: "fenced", Archived: true}
	for _, mutate := range []func(*edgeprotocol.ArchiveAck){func(a *edgeprotocol.ArchiveAck) { a.Archived = false }, func(a *edgeprotocol.ArchiveAck) { a.PayloadHash = "wrong" }, func(a *edgeprotocol.ArchiveAck) { a.Generation++ }, func(a *edgeprotocol.ArchiveAck) { a.Sequence++ }, func(a *edgeprotocol.ArchiveAck) { a.EventID = uuid.NewString() }, func(a *edgeprotocol.ArchiveAck) { a.SessionID = uuid.NewString() }, func(a *edgeprotocol.ArchiveAck) { a.Disposition = "saved" }} {
		bad := ack
		mutate(&bad)
		if err = q.Archive(&event, &bad); err == nil || q.Pending() != 1 {
			t.Fatal("invalid receipt released retained event")
		}
	}
	if err = q.RetryArchive(&event); err != nil {
		t.Fatal(err)
	}
	if next, err = q.NextArchive(); err != nil || next != nil {
		t.Fatal("archive retry has no backoff")
	}
	if err = q.Archive(&event, &ack); err != nil || q.Pending() != 0 {
		t.Fatalf("archive failed: %v", err)
	}
	if err = q.retireCounter(&event); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = q.db.QueryRow(`SELECT count(*) FROM counters`).Scan(&count); err != nil || count != 0 {
		t.Fatal("retired counter retained")
	}
}

func TestExitClassificationKeepsInitiatorAndDoesNotRetainErrorText(t *testing.T) {
	st := &stream{}
	st.recordExit("client_read_failed", &websocket.CloseError{Code: 1006, Text: "secret provider response"})
	st.recordExit("provider_read_failed", errors.New("secondary cancellation"))
	got := st.exit.Load()
	if got.Reason != "client_read_failed" || got.CloseCode != 1006 || got.Timeout {
		t.Fatalf("wrong initiating error: %+v", got)
	}
	data, _ := json.Marshal(got)
	if string(data) != "{\"Reason\":\"client_read_failed\",\"CloseCode\":1006,\"Timeout\":false}" {
		t.Fatal("unexpected diagnostic fields")
	}
}

func TestRunArchivesFencedEventsAndUnblocksDrain(t *testing.T) {
	q, err := OpenQueue(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	event := edgeprotocol.Event{SessionID: uuid.NewString(), Generation: 1, EventID: uuid.NewString(), Kind: "end"}
	if err = q.Append(&event); err != nil {
		t.Fatal(err)
	}
	var archives atomic.Int64
	main := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/edge-control/events":
			w.WriteHeader(http.StatusConflict)
		case "/api/edge-control/heartbeat":
			_ = json.NewEncoder(w).Encode(map[string]string{"mode": "draining"})
		case "/api/edge-control/archive":
			var got edgeprotocol.Event
			if json.NewDecoder(r.Body).Decode(&got) != nil {
				w.WriteHeader(400)
				return
			}
			payload, _ := json.Marshal(got)
			archives.Add(1)
			_ = json.NewEncoder(w).Encode(edgeprotocol.ArchiveAck{SessionID: got.SessionID, Generation: got.Generation, Sequence: got.Sequence, EventID: got.EventID, PayloadHash: edgeprotocol.Hash(string(payload)), Archived: true, Disposition: "fenced"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer main.Close()
	client, err := NewMainClient(main.URL, "identity")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP = main.Client()
	s := &Server{main: client, queue: q, connections: map[string]*stream{}, config: Config{Maximum: 2}}
	s.providerHealthy.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(8 * time.Second)
	for q.Pending() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if q.Pending() != 0 || archives.Load() != 1 {
		t.Fatalf("fenced queue did not drain: pending=%d archives=%d", q.Pending(), archives.Load())
	}
	// Pending counts durable events, not completion of the worker's subsequent
	// counter retirement. Join Run as the real shutdown path does before asserting
	// its cleanup; otherwise a busy CI runner can observe the two writes between.
	cancel()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("archive worker did not finish shutdown")
	}
	var counters int
	if err = q.db.QueryRow(`SELECT count(*) FROM counters`).Scan(&counters); err != nil || counters != 0 {
		t.Fatal("counter leaked after archival")
	}
}
