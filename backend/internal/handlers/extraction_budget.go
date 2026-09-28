package handlers

import (
	"context"
	"sync"
)

type commandBudget struct{ slots chan struct{} }

func (b *commandBudget) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case b.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			b.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *commandBudget) release() { <-b.slots }

// Lazy initialization happens after configuration is loaded at server startup.
var extractionCommandBudget = sync.OnceValue(func() *commandBudget {
	return &commandBudget{slots: make(chan struct{}, knowledgeExtractWorkerCount())}
})

// derivedUploadsPerUser lets one sync overlap a few materials: figure pages
// are read by a model over the network, so a user's uploads mostly wait on
// it rather than on this machine's CPU.
const derivedUploadsPerUser = 3

type derivedUploadBudget struct {
	mu    sync.Mutex
	users map[string]int
	total int
}

var derivedUploads = derivedUploadBudget{users: make(map[string]int)}

// Admit before decoding large bodies. Do not retain a queue of request bodies
// while figures are being read; clients can retry 429 responses.
func (b *derivedUploadBudget) acquire(userID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.users[userID] >= derivedUploadsPerUser ||
		b.total >= knowledgeExtractWorkerCount()*derivedUploadsPerUser {
		return false
	}
	b.users[userID]++
	b.total++
	return true
}

func (b *derivedUploadBudget) release(userID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.users[userID] <= 0 {
		return
	}
	b.users[userID]--
	b.total--
	if b.users[userID] == 0 {
		delete(b.users, userID)
	}
}
