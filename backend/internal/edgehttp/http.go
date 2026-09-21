// Package edgehttp configures HTTPS calls from Edge hosts to the main site.
package edgehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

// NewClient keeps TLS verification and connection reuse, but uses HTTP/1.1.
// Some regional ingress paths accept HTTP/2 GETs while stalling POST bodies.
// All control calls must use the same policy, including the installer.
func NewClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = nil
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	// A previously used default transport may already advertise h2 via ALPN.
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ErrorKind explains transport failures without exposing URLs or credentials
// embedded in an underlying proxy, request or TLS error.
func ErrorKind(err error) string {
	var dns *net.DNSError
	var certificate *tls.CertificateVerificationError
	var network net.Error
	var operation *net.OpError
	switch {
	case errors.Is(err, context.Canceled):
		return "request cancelled"
	case errors.As(err, &dns):
		return "DNS lookup failed"
	case errors.As(err, &certificate):
		return "TLS certificate verification failed"
	case errors.As(err, &network) && network.Timeout():
		return "request timed out"
	case errors.As(err, &operation) && operation.Op == "dial":
		return "connection failed"
	default:
		return "connection interrupted"
	}
}
