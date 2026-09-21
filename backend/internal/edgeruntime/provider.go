package edgeruntime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/gorilla/websocket"
)

// No provider token is cached in config, sent to browsers, or placed in the
// durable return queue. Existing connections use their session lease/budget;
// JWT expiry only controls the next provider handshake.
func (s *Server) dialProvider(ctx context.Context, request edgeprotocol.ProviderCredentialRequest) (*websocket.Conn, *http.Response, error) {
	endpoint := s.config.ProviderURL
	headers := http.Header{}
	if s.config.ProviderAuth == "main" {
		var credential edgeprotocol.ProviderCredential
		if err := s.main.Call(ctx, "provider-credential", request, &credential); err != nil {
			return nil, nil, err
		}
		remaining := time.Until(time.Unix(credential.ExpiresAt, 0))
		if credential.JWT == "" || len(credential.JWT) > 16<<10 || remaining < 5*time.Second || remaining > time.Minute || credential.Training != s.config.Training {
			return nil, nil, errors.New("invalid temporary provider credential")
		}
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, nil, errors.New("invalid provider endpoint")
		}
		q := u.Query()
		q.Set("jwt", credential.JWT)
		u.RawQuery = q.Encode()
		endpoint = u.String()
	} else {
		headers.Set("Authorization", "Bearer "+s.config.ProviderKey)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, response, err := dialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		// Dial errors can contain the URL's JWT. Return a fixed classification only.
		return conn, response, errors.New("provider handshake failed")
	}
	return conn, response, nil
}
