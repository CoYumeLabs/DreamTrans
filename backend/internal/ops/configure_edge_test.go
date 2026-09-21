package ops

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureEdgePreservesIdentityAndVolumesOnDeploymentFailure(t *testing.T) {
	c := testController(t)
	_, _, _ = testEngine(c)
	active := obj(obj(c.state["colors"])["blue"])
	obj(active["contract"])["edge_configuration"] = 1
	image := "example/edge@sha256:" + strings.Repeat("a", 64)
	proxy := "example/proxy@sha256:" + strings.Repeat("b", 64)
	seed := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	c.state["application_env"] = object{"APP_BASE_URL": "https://main.example.test", "JWT_SECRET": "preserve-jwt", "SM_API_KEY": "preserve-provider"}
	dotenv := "COMPOSE_FILE=a:b:c\nEDGE_SIGNING_SEED=" + seed + "\n"
	atomic(filepath.Join(c.root, ".env"), []byte(dotenv), 0o600)
	fallback := c.run
	c.run = func(ctx context.Context, args []string, input string) (string, error) {
		if len(args) > 3 && args[1] == "image" && args[2] == "inspect" {
			return string(marshal([]any{object{"Id": "edge-id"}})), nil
		}
		if len(args) > 2 && args[1] == "create" {
			return "edge-extract", nil
		}
		if len(args) > 3 && args[1] == "cp" {
			save(filepath.Join(args[3], "release.json"), object{"protocol": 1, "state_epoch": 1, "expand_migrations": []any{}, "edge_protocol_max": 2, "container_memory_mb": 256})
			atomic(filepath.Join(args[3], "dreamtransctl"), []byte("fixture"), 0o700)
			return "", nil
		}
		// The main candidate cannot be resolved. Configuration is retained for a
		// retry, while the active container and its original environment stay intact.
		if len(args) > 3 && args[1] == "container" && args[2] == "inspect" && args[3] == "fixture-proxy" {
			return "", fmt.Errorf("fixture publication failure")
		}
		return fallback(ctx, args, input)
	}
	c.persist()
	requireFailure(t, func() { c.configureEdge(&options{image: image, proxyImage: proxy}) }, "operation failed")
	settings := obj(c.state["application_env"])
	if settings["EDGE_SIGNING_SEED"] != seed || settings["EDGE_ROUTING_ENABLED"] != "false" || settings["JWT_SECRET"] != "preserve-jwt" || settings["SM_API_KEY"] != "preserve-provider" {
		t.Fatal("configuration or credentials lost")
	}
	if c.state["active"] != "blue" || c.state["database_volume"] != "real-pg" || c.state["application_volume"] != "real-app" {
		t.Fatal("live deployment mutated")
	}
	if c.environmentMatches("blue") {
		t.Fatal("same-image configuration would be skipped")
	}
	if obj(active["application_env"])["EDGE_SIGNING_SEED"] != nil {
		t.Fatal("old color environment overwritten")
	}
	b, err := os.ReadFile(filepath.Join(c.root, ".env"))
	if err != nil || string(b) != dotenv {
		t.Fatal("dotenv rewritten")
	}
	info, err := os.Stat(filepath.Join(c.path, "state.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("secrets not private")
	}
	before := string(marshal(settings))
	requireFailure(t, func() { c.configureEdge(&options{image: image, proxyImage: proxy}) }, "pending configuration")
	if string(marshal(c.state["application_env"])) != before {
		t.Fatal("retry rotated identity")
	}
}

func TestConfigureEdgeRefusesUnsupportedReleaseAndPrematureRouting(t *testing.T) {
	c := testController(t)
	testEngine(c)
	requireFailure(t, func() { c.configureEdge(&options{}) }, "upgrade the main application first")
	obj(obj(obj(c.state["colors"])["blue"])["contract"])["edge_configuration"] = 1
	c.state["application_env"] = object{"APP_BASE_URL": "https://main.example.test"}
	before := string(marshal(c.state))
	requireFailure(t, func() { c.configureEdge(&options{routing: "on"}) }, "no healthy")
	if string(marshal(c.state)) != before {
		t.Fatal("failed configuration changed state")
	}
}

func TestConfigurationRollbackRestoresEnvironment(t *testing.T) {
	c := testController(t)
	_, route, _ := testEngine(c)
	*route = "green"
	colors := obj(c.state["colors"])
	obj(colors["green"])["application_env"] = object{"EDGE_ROUTING_ENABLED": "false", "JWT_SECRET": "original"}
	obj(colors["blue"])["application_env"] = object{"EDGE_ROUTING_ENABLED": "true", "JWT_SECRET": "original"}
	c.state["application_env"] = cloneObject(obj(obj(colors["blue"])["application_env"]))
	c.rollback()
	if obj(c.state["application_env"])["EDGE_ROUTING_ENABLED"] != "false" || c.state["active"] != "green" {
		t.Fatal("rollback kept new routing configuration")
	}
}
