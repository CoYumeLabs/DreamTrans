package ops

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/joho/godotenv"
)

// configureEdge changes only the recorded application environment, under the
// same lifecycle lock, and publishes it through the normal blue-green checks.
// It never rewrites Compose, .env, credentials, volume names or database data.
func (c *controller) configureEdge(o *options) {
	c.assertDatabase()
	need(c.state.Phase == "ready", "finish the current release with resume/drain before configuring Edge")
	active := c.state.Active
	release := obj(c.state.Colors[active])
	need(number(obj(release["contract"])["edge_configuration"]) >= 1, "upgrade the main application first; its version cannot safely separate Edge management from routing")
	settings := cloneObject(c.state.ApplicationEnv)
	previous := cloneObject(settings)
	need(o.routing == "" || o.routing == "on" || o.routing == "off", "--routing must be on or off")
	c.progress("1/4", "核对主站配置；无需 Cloudflare API Key")
	if o.mainURL != "" {
		settings["APP_BASE_URL"] = strings.TrimRight(o.mainURL, "/")
	}
	need(secureOrigin(str(settings["APP_BASE_URL"])), "configure an HTTPS main origin with --main https://your-main-domain")
	if str(settings["EDGE_SIGNING_SEED"]) == "" {
		// Preserve an operator's existing key even if it was not loaded into the
		// migrated container. Never evaluate dotenv content as shell code.
		if exists(filepath.Join(c.root, ".env")) {
			values, err := godotenv.Read(filepath.Join(c.root, ".env"))
			check(err, "cannot parse existing .env; no configuration changed")
			settings["EDGE_SIGNING_SEED"] = values["EDGE_SIGNING_SEED"]
		}
		if str(settings["EDGE_SIGNING_SEED"]) == "" {
			seed := make([]byte, ed25519.SeedSize)
			_, err := rand.Read(seed)
			check(err, "cannot generate Edge signing seed")
			settings["EDGE_SIGNING_SEED"] = base64.RawStdEncoding.EncodeToString(seed)
		}
		settings["EDGE_ROUTING_ENABLED"] = "false"
	}
	seed, err := base64.RawStdEncoding.DecodeString(str(settings["EDGE_SIGNING_SEED"]))
	need(err == nil && len(seed) == ed25519.SeedSize, "existing Edge signing seed is invalid; retained without rotation")
	if o.routing != "" {
		settings["EDGE_ROUTING_ENABLED"] = "false"
		if o.routing == "on" {
			settings["EDGE_ROUTING_ENABLED"] = "true"
		}
	}
	if o.routing == "on" {
		ready := c.pg(`SELECT count(*) FROM edge_nodes WHERE mode='enabled' AND NOT training AND heartbeat_at>now()-interval '30 seconds' AND protocol_min<=2 AND protocol_max>=2 AND coalesce((metrics->>'healthy')::boolean,false) AND coalesce((metrics->>'load')::float,100)<0.95 AND (SELECT count(*) FROM edge_sessions WHERE node_id=edge_nodes.id AND status<>'closed' AND lease_until>now())<max_connections;`)
		need(strings.TrimSpace(ready) != "0" && strings.TrimSpace(ready) != "", "no healthy, enabled Edge with capacity; install and test a node before enabling routing")
	}
	c.progress("2/4", "固定 Edge 与入口代理版本，保留已有配置")
	if o.image != "" {
		settings["EDGE_RELEASE_IMAGE"] = o.image
	}
	if str(settings["EDGE_RELEASE_IMAGE"]) == "" {
		labels := obj(obj(c.inspect("image", str(release["image"]))["Config"])["Labels"])
		revision := str(labels["org.opencontainers.image.revision"])
		need(regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision), "main image lacks a release revision; specify --image with an immutable Edge digest")
		ref := "ghcr.io/coyumelabs/dreamtrans:edge-" + revision
		c.docker("pull", ref)
		info := c.inspect("image", ref)
		edgeLabels := obj(obj(info["Config"])["Labels"])
		need(str(edgeLabels["org.opencontainers.image.revision"]) == revision && str(edgeLabels["org.opencontainers.image.source"]) == "https://github.com/CoYumeLabs/DreamTrans", "Edge release source/revision differs from the main release")
		settings["EDGE_RELEASE_IMAGE"] = repositoryDigest(info)
	}
	if o.proxyImage != "" {
		settings["EDGE_PROXY_IMAGE"] = o.proxyImage
	}
	if str(settings["EDGE_PROXY_IMAGE"]) == "" {
		settings["EDGE_PROXY_IMAGE"] = repositoryDigest(c.inspect("image", c.state.ProxyImage))
	}
	if o.tunnelImage != "" {
		settings["EDGE_CLOUDFLARED_IMAGE"] = o.tunnelImage
	}
	for _, key := range []string{"EDGE_RELEASE_IMAGE", "EDGE_PROXY_IMAGE", "EDGE_CLOUDFLARED_IMAGE"} {
		value := str(settings[key])
		if key == "EDGE_CLOUDFLARED_IMAGE" && value == "" {
			continue
		}
		need(strings.Contains(value, "@") && immutable.MatchString(value), key+" must be an immutable repository digest")
		id := c.imageID(value)
		if key == "EDGE_RELEASE_IMAGE" {
			c.bundle(id, func(path string) {
				contract := load(filepath.Join(path, "release.json"))
				contractOK(contract, nil)
				need(exists(filepath.Join(path, "dreamtransctl")), "Edge image lacks the Go installer; choose a current Edge release")
				need(number(contract["container_memory_mb"]) > 0 && number(contract["edge_protocol_max"]) >= 2, "selected image is not a compatible Edge release")
			})
		}
	}
	if _, recorded := release["application_env"]; !recorded {
		release["application_env"] = previous
	}
	if !c.environmentMatches(active) {
		fail("pending configuration exists; use upgrade or restore it before changing configuration")
	}
	save(filepath.Join(c.path, "configuration.previous.json"), previous)
	c.state.ApplicationEnv = settings
	c.persist()
	c.progress("3/4", "配置已安全保存；通过蓝绿发布生效，失败可 resume/abort")
	deploy := *o
	deploy.image = str(release["image"])
	c.deploy(&deploy)
	if str(settings["EDGE_ROUTING_ENABLED"]) == "false" {
		c.progress("4/4", "节点管理已配置；普通用户仍走主站。管理员可安装节点并试用")
	} else {
		c.progress("4/4", "Edge 正式调度配置已发布；查看 status 确认排空")
	}
}

func repositoryDigest(info object) string {
	for _, value := range list(info["RepoDigests"]) {
		ref := str(value)
		if strings.Contains(ref, "@") && immutable.MatchString(ref) {
			return ref
		}
	}
	fail("image lacks a repository digest; specify a pinned repository image")
	return ""
}
