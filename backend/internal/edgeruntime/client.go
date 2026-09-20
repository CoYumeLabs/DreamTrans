package edgeruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type MainClient struct {
	URL, Identity string
	HTTP          *http.Client
}
type StatusError int

func (e StatusError) Error() string { return fmt.Sprintf("main-site status %d", int(e)) }
func NewMainClient(base, identity string) (*MainClient, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("main-site URL must be HTTPS")
	}
	return &MainClient{URL: strings.TrimRight(base, "/"), Identity: identity, HTTP: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *MainClient) Call(ctx context.Context, path string, input, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/edge-control/"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Edge "+c.Identity)
	response, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("main-site connection failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return StatusError(response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 512*1024)).Decode(output)
}
