package edgecontrol

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

var immutableImage = regexp.MustCompile(`^[a-zA-Z0-9./:_-]+@sha256:[0-9a-f]{64}$`)

func shellLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func installerPath() string            { return "/usr/share/dreamtrans/edge-install.sh" }
func (s *Service) InstallerHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := os.ReadFile(installerPath())
	if err != nil {
		http.Error(w, "installer unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
func (s *Service) nodeInstallerInfo(ctx context.Context, node, tunnelMode string) (map[string]string, error) {
	if tunnelMode == "" {
		tunnelMode = "existing"
	}
	if tunnelMode != "existing" && tunnelMode != "install" {
		return nil, fmt.Errorf("invalid Tunnel mode")
	}
	maximum, training := 8, false
	if node != "" {
		if err := s.DB.QueryRowContext(ctx, `SELECT max_connections,training FROM edge_nodes WHERE id=$1 AND mode<>'revoked'`, node).Scan(&maximum, &training); err != nil {
			return nil, err
		}
	}
	base := strings.TrimRight(os.Getenv("APP_BASE_URL"), "/")
	image := os.Getenv("EDGE_RELEASE_IMAGE")
	proxy := os.Getenv("EDGE_PROXY_IMAGE")
	if !validEndpoint(base) || !immutableImage.MatchString(image) || !immutableImage.MatchString(proxy) {
		return nil, fmt.Errorf("configure APP_BASE_URL, EDGE_RELEASE_IMAGE and EDGE_PROXY_IMAGE with fixed digests")
	}
	data, err := os.ReadFile(installerPath())
	if err != nil {
		return nil, err
	}
	tunnel := ""
	if tunnelMode == "install" {
		tunnel = os.Getenv("EDGE_CLOUDFLARED_IMAGE")
		if !immutableImage.MatchString(tunnel) {
			return nil, fmt.Errorf("configure a pinned Tunnel client or use an existing Tunnel")
		}
	}
	return installerCommand(base, image, proxy, tunnel, data, maximum, training), nil
}

func installerCommand(base, image, proxy, tunnel string, data []byte, maximum int, training bool) map[string]string {
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	command := "(d=$(mktemp -d); curl -fsSL " + shellLiteral(base+"/api/edge-control/installer") + " -o \"$d/install.sh\" && printf '%s  %s\\n' " + shellLiteral(sum) + " \"$d/install.sh\" | sha256sum -c - && sudo bash \"$d/install.sh\" " + shellLiteral(image) + " --main " + shellLiteral(base) + " --proxy-image " + shellLiteral(proxy) + ")"
	command = strings.TrimSuffix(command, ")") + fmt.Sprintf(" --maximum %d", maximum) + ")"
	if training {
		command = strings.TrimSuffix(command, ")") + " --training)"
	}
	if tunnel != "" {
		command = strings.TrimSuffix(command, ")") + " --tunnel-image " + shellLiteral(tunnel) + ")"
	}
	return map[string]string{"command": command, "sha256": sum, "image": image}
}
