package edgecontrol

import (
	"net/http"
	"os"
	"strings"

	"github.com/dreamtrans/backend/internal/auth"
)

// An absent flag preserves already deployed regional installations. The setup
// command explicitly sets false when provisioning control for the first time.
func RoutingEnabled() bool {
	value, explicit := os.LookupEnv("EDGE_ROUTING_ENABLED")
	return os.Getenv("EDGE_SIGNING_SEED") != "" && (!explicit || strings.EqualFold(value, "true"))
}

// SetupHTTP is also registered before control is configured. No secrets are
// exposed, and an unconfigured administrator page must not become a bare 404.
func SetupHTTP(controlReady bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.GetUserClaims(r.Context())
		if claims == nil || claims.Role != "super_admin" {
			respond(w, nil, ErrUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		missing := []string{}
		if !controlReady {
			missing = append(missing, "节点管理尚未初始化")
		}
		if !validEndpoint(os.Getenv("APP_BASE_URL")) {
			missing = append(missing, "主站 HTTPS 地址")
		}
		for _, item := range []struct{ key, label string }{{"EDGE_RELEASE_IMAGE", "Edge 安装版本"}, {"EDGE_PROXY_IMAGE", "Edge 入口代理版本"}} {
			if !immutableImage.MatchString(os.Getenv(item.key)) {
				missing = append(missing, item.label)
			}
		}
		respond(w, map[string]any{
			"provider_auth": "main", "provider_ready": speechmaticsAccount(false) != "",
			"training_provider_ready": speechmaticsAccount(true) != "",
			"control_ready":           controlReady, "routing_enabled": controlReady && RoutingEnabled(),
			"installer_ready": len(missing) == 0, "missing": missing,
			"release_image":            os.Getenv("EDGE_RELEASE_IMAGE"),
			"tunnel_install_available": immutableImage.MatchString(os.Getenv("EDGE_CLOUDFLARED_IMAGE")),
		}, nil)
	}
}
