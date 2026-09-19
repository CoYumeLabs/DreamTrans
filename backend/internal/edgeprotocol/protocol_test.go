package edgeprotocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"github.com/golang-jwt/jwt/v5"
	"testing"
	"time"
)

func TestGrantBindsNodeOriginBudgetAndExpiry(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	grant := Grant{RegisteredClaims: jwt.RegisteredClaims{Issuer: "dreamtrans-edge", ID: "unique", Audience: jwt.ClaimStrings{"tokyo"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}, NodeID: "tokyo", SessionID: "session", UserID: "user", Origin: "https://yufolo.com", Generation: 1, Provider: "speechmatics", SampleRate: 48000, ApprovedSamples: 1440000, Protocol: 1}
	token, err := Sign(key, &grant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(public, token, "tokyo", "https://yufolo.com"); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"london", "https://yufolo.com"}, {"tokyo", "https://evil.example"}} {
		if _, err = Verify(public, token, pair[0], pair[1]); err == nil {
			t.Fatal("grant accepted by another node/origin")
		}
	}
	grant.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	token, _ = Sign(key, &grant)
	if _, err = Verify(public, token, "tokyo", "https://yufolo.com"); err == nil {
		t.Fatal("expired grant accepted")
	}
}
