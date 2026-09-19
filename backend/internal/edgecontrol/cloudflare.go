package edgecontrol

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// tunnelCrypt uses a purpose-separated key; only the main site can decrypt stored tunnel credentials.
func (s *Service) tunnelCrypt() (cipher.AEAD, error) {
	key := sha256.Sum256(append([]byte("dreamtrans-node-tunnel-v1:"), s.Key.Seed()...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s *Service) sealTunnel(node, token string) (string, error) {
	aead, err := s.tunnelCrypt()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(token), []byte(node))), nil
}
func (s *Service) openTunnel(node, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	aead, err := s.tunnelCrypt()
	if err != nil {
		return "", err
	}
	data, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(data) < aead.NonceSize() {
		return "", errors.New("invalid encrypted tunnel credential")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(node))
	return string(plain), err
}

func cloudflare(ctx context.Context, method, path string, body any, output any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	//nolint:gosec // G704: fixed Cloudflare HTTPS authority; path components are escaped and redirects are disabled.
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("EDGE_CLOUDFLARE_API_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	//nolint:gosec // G704: fixed Cloudflare authority, with credential-bearing redirects refused.
	response, err := client.Do(req)
	if err != nil {
		return errors.New("cloudflare API connection failed")
	}
	defer func() { _ = response.Body.Close() }()
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&envelope); err != nil {
		return errors.New("invalid Cloudflare response")
	}
	if response.StatusCode/100 != 2 || !envelope.Success {
		return fmt.Errorf("cloudflare API rejected operation (%d)", response.StatusCode)
	}
	if output != nil {
		return json.Unmarshal(envelope.Result, output)
	}
	return nil
}

// SetupTunnel resumes by deterministic tunnel name and recorded ID after partial API failures.
// A manual existing-token path needs no Cloudflare account credential at all.
func (s *Service) SetupTunnel(ctx context.Context, actor, node, existingToken string, automatic bool) error {
	var endpoint, tunnel string
	if err := s.DB.QueryRowContext(ctx, `SELECT endpoint,tunnel_id FROM edge_nodes WHERE id=$1 AND mode<>'revoked'`, node).Scan(&endpoint, &tunnel); err != nil {
		return err
	}
	token := existingToken
	if automatic {
		account, zone, zoneName := os.Getenv("EDGE_CLOUDFLARE_ACCOUNT_ID"), os.Getenv("EDGE_CLOUDFLARE_ZONE_ID"), os.Getenv("EDGE_CLOUDFLARE_ZONE_NAME")
		if os.Getenv("EDGE_CLOUDFLARE_API_TOKEN") == "" || account == "" || zone == "" || zoneName == "" {
			return errors.New("main-site scoped Cloudflare credentials are not configured")
		}
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		hostname := parsed.Hostname()
		if !strings.HasSuffix(hostname, "."+zoneName) {
			return errors.New("node hostname is outside configured Cloudflare zone")
		}
		base := "/accounts/" + url.PathEscape(account) + "/cfd_tunnel"
		if tunnel == "" {
			name := "dreamtrans-edge-" + node
			var found []struct {
				ID string `json:"id"`
			}
			if err := cloudflare(ctx, http.MethodGet, base+"?is_deleted=false&name="+url.QueryEscape(name), nil, &found); err != nil {
				return err
			}
			if len(found) > 1 {
				return errors.New("multiple matching tunnels require administrator review")
			}
			if len(found) == 1 {
				tunnel = found[0].ID
			} else {
				var created struct {
					ID string `json:"id"`
				}
				if err := cloudflare(ctx, http.MethodPost, base, map[string]string{"name": name, "config_src": "cloudflare"}, &created); err != nil {
					return err
				}
				tunnel = created.ID
			}
			if _, err = s.DB.ExecContext(ctx, `UPDATE edge_nodes SET tunnel_id=$2 WHERE id=$1`, node, tunnel); err != nil {
				return err
			}
		}
		if err := cloudflare(ctx, http.MethodPut, base+"/"+url.PathEscape(tunnel)+"/configurations", map[string]any{"config": map[string]any{"ingress": []any{map[string]string{"hostname": hostname, "service": "http://dreamtrans:8080"}, map[string]string{"service": "http_status:404"}}}}, nil); err != nil {
			return err
		}
		dnsPath := "/zones/" + url.PathEscape(zone) + "/dns_records"
		var records []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if err := cloudflare(ctx, http.MethodGet, dnsPath+"?name="+url.QueryEscape(hostname), nil, &records); err != nil {
			return err
		}
		target := tunnel + ".cfargotunnel.com"
		if len(records) == 0 {
			if err := cloudflare(ctx, http.MethodPost, dnsPath, map[string]any{"type": "CNAME", "name": hostname, "content": target, "proxied": true}, nil); err != nil {
				return err
			}
		} else if len(records) != 1 || records[0].Type != "CNAME" || records[0].Content != target {
			return errors.New("existing DNS record differs; refusing to overwrite")
		}
		if err := cloudflare(ctx, http.MethodGet, base+"/"+url.PathEscape(tunnel)+"/token", nil, &token); err != nil {
			return err
		}
	}
	if len(token) < 32 || len(token) > 16384 {
		return errors.New("invalid node-specific Tunnel token")
	}
	sealed, err := s.sealTunnel(node, token)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE edge_nodes SET tunnel_token=$2 WHERE id=$1`, node, sealed); err != nil {
		return err
	}
	if err := audit(ctx, tx, node, actor, "tunnel_configured", `{}`); err != nil {
		return err
	}
	return tx.Commit()
}
