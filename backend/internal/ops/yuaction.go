package ops

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (c *controller) yuactionImages(o *options) (string, string) {
	ref := o.image
	if ref == "" {
		ref = "ghcr.io/coyumelabs/dreamtrans-yuaction-backend:latest"
		c.docker("pull", ref)
		ref = str(c.inspect("image", ref)["Id"])
	}
	backend := c.imageID(ref)
	labels := obj(obj(c.inspect("image", backend)["Config"])["Labels"])
	revision := str(labels["org.opencontainers.image.revision"])
	need(regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision), "YuAction image lacks immutable revision metadata")
	front := o.frontendImage
	if front == "" {
		need(str(labels["org.opencontainers.image.source"]) == "https://github.com/CoYumeLabs/DreamTrans", "custom YuAction releases require --frontend-image")
		front = "ghcr.io/coyumelabs/dreamtrans-yuaction-frontend:sha-" + revision
		c.docker("pull", front)
		front = str(c.inspect("image", front)["Id"])
	}
	frontend := c.imageID(front)
	frontLabels := obj(obj(c.inspect("image", frontend)["Config"])["Labels"])
	need(str(frontLabels["org.opencontainers.image.revision"]) == revision, "YuAction frontend/backend revisions differ")
	return backend, frontend
}

func (c *controller) yuactionContract(image string) object {
	var contract object
	c.bundle(image, func(dir string) {
		contract = load(filepath.Join(dir, "release.json"))
		contractOK(contract, nil)
		need(str(contract["component"]) == "yuaction" && number(contract["recording_handoff"]) == 1, "release lacks YuAction recording handoff support")
		c.memory(contract)
		files, err := filepath.Glob(filepath.Join(dir, "migrations", "*.sql"))
		check(err, "cannot inspect YuAction migrations")
		need(len(files) > 0, "missing YuAction migrations")
		sums := object{}
		for _, file := range files {
			name := filepath.Base(file)
			need(contains(contract["expand_migrations"], name), "YuAction migration lacks expand-only declaration")
			sums[name] = hashFile(file)
		}
		applied := object{}
		if c.state != nil {
			applied = c.state.YuactionSchema
		}
		for name, sum := range applied {
			need(sums[name] == sum, "YuAction migration differs from applied release: "+name)
		}
		contract["schema"] = sums
	})
	return contract
}

func (c *controller) deployYuAction(o *options) {
	c.assertDatabase()
	if c.state.Active == "" {
		c.resumeYuActionInitial()
	}
	phase := c.state.Phase
	need(phase == "ready" || phase == "draining", "unfinished YuAction release; use resume/abort/rollback")
	image, frontend := c.yuactionImages(o)
	active := c.state.Active
	previous := obj(c.state.Colors[active])
	if str(previous["image"]) == image && str(previous["frontend_image"]) == frontend {
		if phase == "draining" {
			c.finishRelease(o)
		}
		return
	}
	contract := c.yuactionContract(image)
	contractOK(contract, obj(previous["contract"]))
	target := "blue"
	if active == "blue" {
		target = "green"
	}
	c.state.CandidateFrontend = frontend
	c.persist()
	c.startYuActionColor(target, image, contract)
	if o.pause {
		c.progress("暂停", "候选已验证，执行 resume 切换")
		return
	}
	c.switchColor(target)
	c.observe(o.observe)
	c.finishRelease(o)
}

func (c *controller) writeYuActionRoute(color string) {
	need(color == "blue" || color == "green", "invalid YuAction color")
	// Nginx reload keeps accepted WebSockets alive on the old backend; only new
	// requests use the new color. Frontend and API change as a single route revision.
	config := `worker_processes auto;
error_log /dev/stderr warn;
pid /tmp/nginx.pid;
events { worker_connections 4096; }
http {
 access_log off;
 map $http_upgrade $connection_upgrade { default upgrade; '' close; }
 map $http_x_forwarded_proto $forwarded_scheme { default $scheme; https https; }
 server {
  listen 8080;
  client_max_body_size 11m;
  location = /_release { default_type text/plain; return 200 "COLOR"; }
  location / {
   resolver 127.0.0.11 valid=1s ipv6=off;
   set $target FRONTEND:80;
   if ($uri ~ ^/(api/|healthz$|readyz$)) { set $target BACKEND:18083; }
   proxy_pass http://$target;
   proxy_http_version 1.1;
   proxy_set_header Host $http_host;
   proxy_set_header X-Real-IP $remote_addr;
   proxy_set_header X-Forwarded-Proto $forwarded_scheme;
   proxy_set_header Upgrade $http_upgrade;
   proxy_set_header Connection $connection_upgrade;
   proxy_buffering off;
   proxy_read_timeout 24h;
   proxy_send_timeout 24h;
   proxy_next_upstream off;
  }
 }
}
`
	config = strings.NewReplacer("COLOR", color, "FRONTEND", c.name(color+"-frontend"), "BACKEND", c.name(color)).Replace(config)
	atomic(filepath.Join(c.path, "proxy", "nginx.conf"), []byte(config), 0o644)
}

func (c *controller) startYuActionColor(color, image string, contract object) {
	colors := c.state.Colors
	if c.containerExists(color) {
		release, recorded := colors[color]
		need(recorded, "unrecorded YuAction container occupies candidate name")
		info := c.inspect("container", c.name(color))
		need(str(info["Image"]) == str(obj(release)["image"]), "YuAction candidate identity changed")
		if yes(obj(info["State"])["Running"]) {
			need(yes(c.control(color, "status")["drained"]), "previous YuAction color still owns work")
			c.docker("stop", "--timeout", "-1", c.name(color))
		}
		c.docker("rm", c.name(color))
	}
	if c.containerExists(color + "-frontend") {
		info := c.inspect("container", c.name(color+"-frontend"))
		need(str(info["Image"]) == str(obj(colors[color])["frontend_image"]), "YuAction frontend identity changed")
		c.docker("stop", "--timeout", "-1", c.name(color+"-frontend"))
		c.docker("rm", c.name(color+"-frontend"))
	}
	dir := filepath.Join(c.path, color)
	mkdir(dir)
	check(os.Chown(dir, 10001, 10001), "cannot assign deployment directory")
	atomic(filepath.Join(dir, "mode"), []byte("standby\n"), 0o600)
	check(os.Chown(filepath.Join(dir, "mode"), 10001, 10001), "cannot assign deployment state")
	settings := cloneObject(c.state.ApplicationEnv)
	settings["DREAMTRANS_DEPLOYMENT_MODE"] = "standby"
	settings["DREAMTRANS_DEPLOYMENT_STATE"] = "/deployment/mode"
	settings["LISTEN_ADDR"] = "0.0.0.0:18083"
	atomic(filepath.Join(dir, "application.env"), envBytes(settings), 0o600)
	colors[color] = object{"image": image, "frontend_image": c.state.CandidateFrontend, "contract": contract, "application_env": cloneObject(c.state.ApplicationEnv)}
	c.state.Phase = "candidate"
	c.state.Target = color
	c.persist()
	c.docker("run", "-d", "--name", c.name(color), "--restart", "unless-stopped", "--network", c.state.DatabaseNetwork, "--env-file", filepath.Join(dir, "application.env"), "--mount", "type=bind,src="+dir+",dst=/deployment", "--label", "dreamtrans.release="+c.state.Prefix, image)
	c.docker("network", "connect", c.state.Network, c.name(color))
	c.ensureYuActionFrontend(color)
	c.smoke(color)
	c.state.YuactionSchema = obj(contract["schema"])
	c.persist()
}

func (c *controller) ensureYuActionFrontend(color string) {
	image := str(obj(c.state.Colors[color])["frontend_image"])
	name := color + "-frontend"
	if c.containerExists(name) {
		info := c.inspect("container", c.name(name))
		need(str(info["Image"]) == image, "YuAction frontend identity differs from intent")
		if !yes(obj(info["State"])["Running"]) {
			c.docker("start", c.name(name))
		}
		return
	}
	c.docker("run", "-d", "--name", c.name(name), "--restart", "unless-stopped", "--network", c.state.Network, "-e", "YUACTION_BACKEND_HOST="+c.name(color), "--label", "dreamtrans.release="+c.state.Prefix, image)
}
func (c *controller) recoverYuActionCandidate() {
	color := c.state.Target
	need(color != c.state.Active && (color == "blue" || color == "green"), "invalid YuAction candidate")
	release := obj(c.state.Colors[color])
	if !c.containerExists(color) {
		c.state.CandidateFrontend = str(release["frontend_image"])
		c.startYuActionColor(color, str(release["image"]), obj(release["contract"]))
		return
	}
	info := c.inspect("container", c.name(color))
	need(str(info["Image"]) == str(release["image"]) && hasMount(info, "/deployment", filepath.Join(c.path, color)), "YuAction candidate differs from intent")
	if !yes(obj(info["State"])["Running"]) {
		c.docker("start", c.name(color))
	}
	if _, ok := networks(info)[c.state.Network]; !ok {
		c.docker("network", "connect", c.state.Network, c.name(color))
	}
	c.ensureYuActionFrontend(color)
	c.smoke(color)
}

func (c *controller) initYuAction(o *options) {
	if c.state != nil {
		need(c.yuaction(), "installation belongs to another product")
		if c.state.Active == "" {
			c.resumeYuActionInitial()
			return
		}
		c.progress("✓", "已有 YuAction 蓝绿安装，请使用 upgrade")
		return
	}
	need(o.maintenance, "首次采用蓝绿协议需要 --maintenance；已有旧客户端无法凭空获得交接协议")
	app, front, db := c.inspect("container", o.app), c.inspect("container", o.frontend), c.inspect("container", o.database)
	settings := env(app)
	need(str(settings["DATABASE_URL"]) != "" && len(str(settings["YUACTION_CREATOR_KEY"])) >= 32, "YuAction requires persistent PostgreSQL and its existing creator key")
	network := o.databaseNetwork
	shared := []string{}
	for n := range networks(app) {
		if _, ok := networks(db)[n]; ok {
			shared = append(shared, n)
		}
	}
	if network == "" && len(shared) == 1 {
		network = shared[0]
	}
	matched := false
	for _, n := range shared {
		matched = matched || n == network
	}
	need(matched, "specify YuAction's shared --database-network")
	bindings := list(obj(obj(front["NetworkSettings"])["Ports"])["80/tcp"])
	need(len(bindings) == 1 && str(obj(bindings[0])["HostPort"]) == fmt.Sprint(o.port), "existing frontend must own requested YuAction entrance port")
	image, frontend := c.yuactionImages(o)
	contract := c.yuactionContract(image)
	proxy := c.imageID(o.proxyImage)
	databaseEnv := object{"PGUSER": env(db)["POSTGRES_USER"], "PGDATABASE": env(db)["POSTGRES_DB"], "PGPASSWORD": env(db)["POSTGRES_PASSWORD"]}
	if parsed, err := url.Parse(str(settings["DATABASE_URL"])); err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		databaseEnv["PGUSER"] = parsed.User.Username()
		databaseEnv["PGPASSWORD"], _ = parsed.User.Password()
		databaseEnv["PGDATABASE"] = strings.TrimPrefix(parsed.Path, "/")
	} else {
		for _, k := range []string{"PGUSER", "PGDATABASE", "PGPASSWORD"} {
			if str(settings[k]) != "" {
				databaseEnv[k] = settings[k]
			}
		}
	}
	prefix := fmt.Sprintf("yuaction-%x", sha256.Sum256([]byte(c.root)))[:21]
	c.state = stateFromObject(object{"format": 1, "role": "yuaction", "prefix": prefix, "network": prefix + "-entry", "database_network": network, "database_id": db["Id"], "database_volume": c.dataMount(db, "/var/lib/postgresql/data"), "database_env": databaseEnv, "application_env": settings, "legacy_id": app["Id"], "legacy_frontend": front["Id"], "proxy_image": proxy, "bind": obj(bindings[0])["HostIp"], "port": o.port, "colors": object{}, "phase": "initializing", "initial_image": image, "initial_contract": contract, "candidate_frontend": frontend})
	c.persist()
	c.resumeYuActionInitial()
}
func (c *controller) resumeYuActionInitial() {
	c.assertDatabase()
	// Fail closed: this one-time conversion must never kill a pre-protocol
	// browser recording. Subsequent upgrade/rollback uses cooperative migration.
	schema := "public"
	if strings.Contains(str(c.state.ApplicationEnv["DATABASE_URL"]), "search_path=yuaction") {
		schema = "yuaction"
	}
	need(c.pg("SELECT count(*) FROM "+schema+".rooms WHERE state->>'transcription'='recording';") == "0", "legacy recording exists; kept both existing containers running; finish legacy recording before initial conversion")
	c.ensureEntryNetwork()
	if !c.containerExists("blue") {
		c.startYuActionColor("blue", c.state.InitialImage, c.state.InitialContract)
	} else {
		c.state.Target = "blue"
		c.recoverYuActionCandidate()
	}
	for _, key := range []string{"legacy_frontend", "legacy_id"} {
		c.docker("update", "--restart=no", str(c.state.publicValue(key)))
		c.docker("stop", "--timeout", "-1", str(c.state.publicValue(key)))
	}
	c.control("blue", "active")
	c.ensureProxy("blue")
	c.state.Active = "blue"
	c.state.Phase = "ready"
	c.persist()
	c.progress("✓", "YuAction 固定端口入口已采用蓝绿；后续升级与回滚保持录音采集")
}

// A linked installation follows the same immutable commit as main. Standalone
// YuAction installations keep their own port and independent lifecycle command.
func (c *controller) upgradeYuActionCompanion(o *options) {
	root := filepath.Join(c.root, "yuaction")
	if !exists(filepath.Join(root, ".bluegreen", "state.json")) {
		return
	}
	active := obj(c.state.Colors[c.state.Active])
	labels := obj(obj(c.inspect("image", str(active["image"]))["Config"])["Labels"])
	revision := str(labels["org.opencontainers.image.revision"])
	need(regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision) && str(labels["org.opencontainers.image.source"]) == "https://github.com/CoYumeLabs/DreamTrans", "linked YuAction requires a verified official main revision; use its independent command for custom releases")
	ref := "ghcr.io/coyumelabs/dreamtrans-yuaction-backend:sha-" + revision
	c.docker("pull", ref)
	image := str(c.inspect("image", ref)["Id"])
	c.progress("YuAction", "主站已接流；使用同一提交自动迁移 YuAction 录音")
	err := Run(c.ctx, []string{"yuaction", "--dir", root, "upgrade", "--image", image, "--observe", fmt.Sprint(o.observe), "--drain-timeout", fmt.Sprint(o.drainTimeout)}, c.out, c.errOut)
	check(err, "main is upgraded; YuAction release remains recoverable with dreamtransctl yuaction --dir DIR resume")
}
