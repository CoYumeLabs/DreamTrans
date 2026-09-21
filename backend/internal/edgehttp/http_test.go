package edgehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestControlPOSTUsesHTTP1WhenHTTP2GETWorks(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Observed-Protocol", r.Proto)
			return
		}
		if r.ProtoMajor == 2 {
			// Reproduce the observed ingress failure: GET succeeds, POST stalls.
			<-r.Context().Done()
			return
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"event":"test"}` || r.Header.Get("Authorization") != "Edge fixture" {
			t.Error("control request body or identity changed")
		}
		posts.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	baseline := server.Client()
	t.Cleanup(baseline.CloseIdleConnections)
	response, err := baseline.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.Header.Get("Observed-Protocol") != "HTTP/2.0" {
		t.Fatal("fixture must offer a working HTTP/2 GET")
	}
	client := NewClient(2 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs = baseline.Transport.(*http.Transport).TLSClientConfig.RootCAs
	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{"event":"test"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Edge fixture")
		response, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || response.ProtoMajor != 1 {
			t.Fatal("control POST did not use HTTP/1.1")
		}
	}
	if posts.Load() != 2 {
		t.Fatal("lost or replayed a control POST")
	}
}

func TestControlClientStillVerifiesTLSAndRefusesRedirects(t *testing.T) {
	var redirected atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/sink", http.StatusTemporaryRedirect)
			return
		}
		redirected.Store(true)
	}))
	t.Cleanup(server.Close)
	client := NewClient(2 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Get(server.URL + "/redirect")
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("accepted an untrusted main-site certificate")
	}
	if ErrorKind(err) != "TLS certificate verification failed" {
		t.Fatal("certificate failure was not classified")
	}
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	response, err = client.Post(server.URL+"/redirect", "application/json", strings.NewReader(`{"token":"private"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || redirected.Load() {
		t.Fatal("followed a credential-bearing redirect")
	}
}

func TestTransportErrorsDoNotExposeCredentials(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "request cancelled"},
		{context.DeadlineExceeded, "request timed out"},
		{&net.DNSError{Name: "secret.example", Err: "secret"}, "DNS lookup failed"},
		{&tls.CertificateVerificationError{Err: errors.New("secret")}, "TLS certificate verification failed"},
		{&net.OpError{Op: "dial", Err: errors.New("secret")}, "connection failed"},
		{errors.New("secret"), "connection interrupted"},
	} {
		err := &url.Error{Op: "Post", URL: "https://secret:secret@main.invalid", Err: test.err}
		if got := ErrorKind(err); got != test.want || strings.Contains(got, "secret") {
			t.Fatalf("unexpected safe error category: %s", got)
		}
	}
}
