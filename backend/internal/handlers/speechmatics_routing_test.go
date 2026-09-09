package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
)

func TestSpeechmaticsRoutingWithoutProgrammeUsesTheOnlyKey(t *testing.T) {
	t.Setenv("SM_API_KEY", "training-key")
	t.Setenv(speechmaticsNoTrainingKeyEnv, "")
	routing, err := loadSpeechmaticsRouting()
	if err != nil {
		t.Fatal(err)
	}
	if routing.available() || TrainingProgramAvailable() {
		t.Fatal("program reported available without a no-training key")
	}
	routing.lookup = func(context.Context, string) (bool, error) { return false, nil }
	claims := &auth.UserClaims{UserID: "u1"}
	if !routing.useTrainingRoute(context.Background(), claims) || !routing.useTrainingRoute(context.Background(), nil) {
		t.Fatal("single-key deployment must keep using SM_API_KEY")
	}
	if routing.key(false) != "training-key" {
		t.Fatalf("key(false) = %q, want the only key", routing.key(false))
	}
	// Even with one key the ledger must price the stream from a decision made
	// at start, or a gift-funded stream loses its promotion discount and the
	// close-time refund never runs.
	proxy, err := NewSpeechmaticsProxyHandler(nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy.billing = &speechmaticsBillingStub{route: billing.RouteDecision{GiftFunded: true, Reason: "gift_balance", DiscountsSnapshotted: true}}
	gen, _, route := proxy.tokenGeneratorFor(context.Background(), claims)
	if gen != proxy.tokenGenerator || route == nil || !route.GiftFunded {
		t.Fatalf("single-key stream lost its route decision: gen=%v route=%+v", gen == proxy.tokenGenerator, route)
	}
}

func TestSpeechmaticsRoutingOnlyExplicitOptInReachesTrainingAccount(t *testing.T) {
	t.Setenv("SM_API_KEY", "training-key")
	t.Setenv(speechmaticsNoTrainingKeyEnv, "clean-key")
	routing, err := loadSpeechmaticsRouting()
	if err != nil {
		t.Fatal(err)
	}
	if !routing.available() {
		t.Fatal("program should be available")
	}
	ctx := context.Background()
	if routing.useTrainingRoute(ctx, nil) {
		t.Fatal("anonymous audio must not reach the training account")
	}
	if routing.useTrainingRoute(ctx, &auth.UserClaims{UserID: "u1"}) {
		t.Fatal("without a lookup nobody is opted in")
	}
	answers := map[string]bool{"joined": true, "declined": false}
	routing.lookup = func(_ context.Context, userID string) (bool, error) {
		if userID == "broken" {
			return true, errors.New("db down")
		}
		return answers[userID], nil
	}
	if !routing.useTrainingRoute(ctx, &auth.UserClaims{UserID: "joined"}) {
		t.Fatal("opted-in user should use the training account")
	}
	if routing.useTrainingRoute(ctx, &auth.UserClaims{UserID: "declined"}) {
		t.Fatal("declined user reached the training account")
	}
	if routing.useTrainingRoute(ctx, &auth.UserClaims{UserID: "broken"}) {
		t.Fatal("a lookup failure must fall back to the no-training account")
	}
	if routing.key(true) != "training-key" || routing.key(false) != "clean-key" {
		t.Fatalf("keys = %q / %q", routing.key(true), routing.key(false))
	}
}

func TestLiveAndBatchHandlersRouteNonMembersToTheNoTrainingAccount(t *testing.T) {
	t.Setenv("SM_API_KEY", "training-key")
	t.Setenv(speechmaticsNoTrainingKeyEnv, "clean-key")
	lookup := func(_ context.Context, userID string) (bool, error) { return userID == "joined", nil }
	ctx := context.Background()
	joined := &auth.UserClaims{UserID: "joined"}
	declined := &auth.UserClaims{UserID: "declined"}

	proxy, err := NewSpeechmaticsProxyHandler(nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy.SetTrainingOptInLookup(lookup)
	if proxy.noTrainingTokenGenerator == nil || proxy.noTrainingTokenGenerator == proxy.tokenGenerator {
		t.Fatal("proxy did not build a separate generator for the no-training account")
	}
	if gen, training, _ := proxy.tokenGeneratorFor(ctx, declined); training || gen != proxy.noTrainingTokenGenerator {
		t.Fatal("declined user's live stream would mint its key on the training account")
	}
	if gen, training, _ := proxy.tokenGeneratorFor(ctx, nil); training || gen != proxy.noTrainingTokenGenerator {
		t.Fatal("anonymous live stream would mint its key on the training account")
	}
	if gen, training, _ := proxy.tokenGeneratorFor(ctx, joined); !training || gen != proxy.tokenGenerator {
		t.Fatal("joined user's live stream did not use the training account")
	}

	token, err := NewTokenHandler()
	if err != nil {
		t.Fatal(err)
	}
	token.SetTrainingOptInLookup(lookup)
	if token.tokenGeneratorFor(ctx, declined) != token.noTrainingTokenGen {
		t.Fatal("legacy token endpoint would mint a declined user's key on the training account")
	}
	if token.tokenGeneratorFor(ctx, joined) != token.tokenGen {
		t.Fatal("legacy token endpoint did not use the training account for a joined user")
	}

	batch, err := NewBatchTranscribeHandler(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch.SetTrainingOptInLookup(lookup)
	if batch.noTrainingClient == nil || batch.noTrainingClient == batch.trainingClient {
		t.Fatal("batch handler did not build a separate client for the no-training account")
	}
	if client := batch.clientFor(false); client != batch.noTrainingClient {
		t.Fatal("declined batch upload would go to the training account")
	}
	if client := batch.clientFor(true); client != batch.trainingClient {
		t.Fatal("joined batch upload did not go to the training account")
	}
}
