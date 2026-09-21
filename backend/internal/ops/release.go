package ops

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

func (c *controller) migrate(bundle string, contract object) {
	c.assertDatabase()
	if c.edge() {
		contractOK(contract, nil)
		c.progress("3/8", "Edge 保留独立回传队列，无主站数据库迁移")
		return
	}
	applied := object{}
	for _, line := range strings.Split(c.pg("SELECT version,checksum FROM schema_migrations ORDER BY version;"), "\n") {
		if line == "" {
			continue
		}
		v, sum, ok := strings.Cut(line, "|")
		need(ok, "invalid migration ledger")
		applied[v] = sum
	}
	paths, e := filepath.Glob(filepath.Join(bundle, "migrations", "*.sql"))
	check(e, "invalid migration bundle")
	need(len(paths) > 0, "empty migration bundle")
	migrations := object{}
	pending := 0
	for _, path := range paths {
		name := filepath.Base(path)
		migrations[name] = hashFile(path)
		if _, ok := applied[name]; !ok {
			need(contains(contract["expand_migrations"], name), "pending migration lacks expand-only compatibility: "+name)
			pending++
		}
	}
	for name, sum := range applied {
		need(migrations[name] == sum, "applied migration differs from release: "+name)
	}
	c.progress("3/8", fmt.Sprintf("执行兼容迁移：%d 个；保留数据库与卷", pending))
	stage := filepath.Join(c.path, "migration")
	check(os.RemoveAll(stage), "cannot clear derived migration staging directory")
	mkdir(filepath.Join(stage, "migrations"))
	for _, path := range paths {
		copyFile(path, filepath.Join(stage, "migrations", filepath.Base(path)), 0o600)
	}
	copyFile(filepath.Join(bundle, "migrate.sh"), filepath.Join(stage, "migrate.sh"), 0o700)
	args := make([]string, 0, 16+2*len(obj(c.state["database_env"])))
	args = append(args, "run", "--rm", "--network", str(c.state["database_network"]), "--mount", "type=bind,src="+stage+",dst=/release,readonly")
	args = append(args, envArgs(obj(c.state["database_env"]))...)
	args = append(args, "-e", "MIGRATIONS_DIR=/release/migrations", "--entrypoint", "/bin/sh", str(c.state["database_image"]), "/release/migrate.sh")
	c.docker(args...)
	c.state["schema"] = migrations
	c.persist()
}
func (c *controller) writeRoute(color string) {
	if c.yuaction() {
		c.writeYuActionRoute(color)
		return
	}
	need(color == "blue" || color == "green", "invalid route color")
	config := `worker_processes auto;
error_log /dev/stderr warn;
pid /tmp/nginx.pid;
events { worker_connections 4096; }
http {
 access_log off;
 map $http_upgrade $connection_upgrade { default upgrade; '' close; }
 server {
  listen 8080;
  client_max_body_size 110m;
  location = /_release { default_type text/plain; return 200 "COLOR"; }
  location / {
   resolver 127.0.0.11 valid=5s ipv6=off;
   set $backend BACKEND:8080;
   proxy_pass http://$backend;
   proxy_http_version 1.1;
   proxy_set_header Host $host;
   proxy_set_header Upgrade $http_upgrade;
   proxy_set_header Connection $connection_upgrade;
   proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
   proxy_set_header X-Forwarded-Proto $http_x_forwarded_proto;
   proxy_buffering off;
   proxy_read_timeout 24h;
   proxy_send_timeout 24h;
   proxy_next_upstream off;
  }
 }
}
`
	config = strings.ReplaceAll(strings.ReplaceAll(config, "COLOR", color), "BACKEND", c.name(color))
	atomic(filepath.Join(c.path, "proxy", "nginx.conf"), []byte(config), 0o644)
}
func (c *controller) routeColor() string {
	return c.docker("exec", c.name("proxy"), "wget", "-qO-", "http://127.0.0.1:8080/_release")
}
func hasMount(info object, destination, source string) bool {
	for _, v := range list(info["Mounts"]) {
		m := obj(v)
		if str(m["Destination"]) == destination && str(m["Source"]) == source {
			return true
		}
	}
	return false
}
func (c *controller) syncEntry() {
	if c.edge() || c.yuaction() {
		return
	}
	c.assertDatabase()
	network := str(c.state["database_network"])
	_, ok := networks(c.inspect("container", str(c.state["database_id"])))[network]
	need(ok, "recorded database network is no longer attached")
	proxy := c.inspect("container", c.name("proxy"))
	_, entry := networks(proxy)[str(c.state["network"])]
	need(str(proxy["Image"]) == str(c.state["proxy_image"]) && hasMount(proxy, "/release", filepath.Join(c.path, "proxy")) && entry, "proxy identity differs from recorded installation")
	members := obj(c.inspect("network", network)["Containers"])
	for id := range members {
		if id == str(proxy["Id"]) {
			continue
		}
		other := c.inspect("container", id)
		alias := obj(networks(other)[network])["Aliases"]
		need(!yes(obj(other["State"])["Running"]) || strings.TrimPrefix(str(other["Name"]), "/") != "dreamtrans" && !contains(alias, "dreamtrans"), "dreamtrans alias is owned by another running container")
	}
	if attached, ok := networks(proxy)[network]; ok {
		need(contains(obj(attached)["Aliases"], "dreamtrans"), "proxy lacks its stable alias; review before reconnecting")
		return
	}
	c.docker("network", "connect", "--alias", "dreamtrans", network, str(proxy["Id"]))
	need(contains(obj(networks(c.inspect("container", str(proxy["Id"])))[network])["Aliases"], "dreamtrans"), "stable internal alias was not attached")
}
func (c *controller) ensureProxy(color string) {
	c.writeRoute(color)
	if c.containerExists("proxy") {
		p := c.inspect("container", c.name("proxy"))
		need(str(p["Image"]) == str(c.state["proxy_image"]) && hasMount(p, "/release", filepath.Join(c.path, "proxy")), "existing proxy differs from recorded installation")
		if !yes(obj(p["State"])["Running"]) {
			c.docker("start", c.name("proxy"))
		} else {
			c.reloadProxy()
		}
	} else {
		c.docker("run", "-d", "--name", c.name("proxy"), "--restart", "unless-stopped", "--network", str(c.state["network"]), "--network-alias", "dreamtrans", "-p", fmt.Sprintf("%s:%d:8080", str(c.state["bind"]), number(c.state["port"])), "--mount", "type=bind,src="+filepath.Join(c.path, "proxy")+",dst=/release,readonly", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", str(c.state["proxy_image"]), "nginx", "-g", "daemon off;", "-c", "/release/nginx.conf")
	}
	c.syncEntry()
	for range 30 {
		if attempt(func() { need(c.routeColor() == color, "route not ready") }) == nil {
			return
		}
		c.sleep(time.Second)
	}
	fail("proxy route did not become ready; resume initialization")
}
func (c *controller) reloadProxy() {
	c.docker("exec", c.name("proxy"), "nginx", "-t", "-c", "/release/nginx.conf")
	c.docker("exec", c.name("proxy"), "nginx", "-s", "reload", "-c", "/release/nginx.conf")
}
func (c *controller) switchColor(color string) {
	c.state["phase"] = "switching"
	c.state["target"] = color
	c.persist()
	old := str(c.state["active"])
	c.control(color, "active")
	c.writeRoute(color)
	err := attempt(func() {
		c.reloadProxy()
		for range 20 {
			if c.routeColor() == color {
				return
			}
			c.sleep(500 * time.Millisecond)
		}
		fail("proxy did not acknowledge new route")
	})
	if err != nil {
		if old != "" {
			c.writeRoute(old)
			c.reloadProxy()
		}
		c.control(color, "draining")
		fail(err.Error())
	}
	c.state["active"] = color
	if settings, ok := obj(obj(c.state["colors"])[color])["application_env"]; ok && !c.edge() {
		c.state["application_env"] = cloneObject(obj(settings))
	}
	c.state["previous"] = nullable(old)
	c.state["phase"] = "observing"
	c.state["drain_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	delete(c.state, "handoff_status")
	c.persist()
	if old != "" && old != color {
		c.control(old, "draining")
	}
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (c *controller) startColor(color, image string, contract object) {
	if c.yuaction() {
		c.startYuActionColor(color, image, contract)
		return
	}
	if c.edge() {
		c.startEdgeColor(color, image, contract)
		return
	}
	name := c.name(color)
	colors := obj(c.state["colors"])
	if c.containerExists(color) {
		_, recorded := colors[color]
		need(recorded, "candidate name is already owned by an unrecorded container")
	}
	if _, ok := colors[color]; ok && c.containerExists(color) {
		existing := c.inspect("container", name)
		need(str(existing["Image"]) == str(obj(colors[color])["image"]), "inactive container image differs from recorded installation")
		if yes(obj(existing["State"])["Running"]) {
			need(yes(c.control(color, "status")["drained"]), "inactive color still owns work; drain before another release")
			c.docker("stop", "--timeout", "-1", name)
		}
		c.docker("rm", name)
	}
	dir := filepath.Join(c.path, color)
	mkdir(dir)
	check(os.Chown(dir, 10001, 10001), "cannot assign deployment directory")
	atomic(filepath.Join(dir, "mode"), []byte("standby\n"), 0o600)
	check(os.Chown(filepath.Join(dir, "mode"), 10001, 10001), "cannot assign mode file")
	settings := object{}
	for k, v := range obj(c.state["application_env"]) {
		settings[k] = v
	}
	for k, v := range (object{"RAG_STORAGE": "postgres", "ALLOW_ANONYMOUS_API": "false", "DREAMTRANS_DEPLOYMENT_MODE": "standby", "DREAMTRANS_DEPLOYMENT_STATE": "/deployment/mode", "PORT": "8080"}) {
		settings[k] = v
	}
	envFile := filepath.Join(dir, "application.env")
	atomic(envFile, envBytes(settings), 0o600)
	colors[color] = object{"image": image, "contract": contract, "application_env": cloneObject(obj(c.state["application_env"]))}
	c.state["phase"] = "candidate"
	c.state["target"] = color
	c.persist()
	c.docker("run", "-d", "--name", name, "--restart", "unless-stopped", "--network", str(c.state["database_network"]), "--env-file", envFile, "--mount", "type=volume,src="+str(c.state["application_volume"])+",dst=/app/data", "--mount", "type=bind,src="+dir+",dst=/deployment", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", "--label", "dreamtrans.release="+str(c.state["prefix"]), image)
	c.docker("network", "connect", str(c.state["network"]), name)
	c.progress("4/8", "启动 "+color+"；待命实例不领取后台任务")
	c.smoke(color)
}
func (c *controller) smoke(color string) {
	c.waitReady(color)
	if c.yuaction() {
		c.control(color, "canary")
		defer c.control(color, "standby")
		body := c.docker("exec", c.name(color), "wget", "-qO-", "http://127.0.0.1:18083/api/config")
		need(strings.Contains(body, "yufoloConnected"), "YuAction smoke check failed")
		return
	}
	if c.edge() {
		return
	}
	c.control(color, "canary")
	func() {
		defer c.control(color, "standby")
		for path, marker := range map[string]string{"/pro": "pro-root", "/api/system/access": "auth"} {
			body := c.docker("exec", c.name(color), "wget", "-qO-", "http://127.0.0.1:8080"+path)
			need(strings.Contains(body, marker), "candidate functional smoke check failed")
		}
	}()
	c.progress("5/8", "就绪与小范围功能检查完成")
}
func (c *controller) initialColor(image string, contract object) {
	if !c.containerExists("blue") {
		c.startColor("blue", image, contract)
		return
	}
	current := c.inspect("container", c.name("blue"))
	need(str(current["Image"]) == image, "initial instance image differs from intent")
	if c.edge() {
		need(hasMount(current, "/spool", filepath.Join(c.path, "blue", "spool")), "initial Edge journal differs from intent")
	} else {
		need(c.dataMount(current, "/app/data") == str(c.state["application_volume"]), "initial application volume differs from intent")
	}
	if _, ok := networks(current)[str(c.state["network"])]; !ok {
		c.docker("network", "connect", str(c.state["network"]), c.name("blue"))
	}
	if !yes(obj(current["State"])["Running"]) {
		c.docker("start", c.name("blue"))
	}
	colors := obj(c.state["colors"])
	if _, ok := colors["blue"]; !ok {
		colors["blue"] = object{"image": image, "contract": contract}
	}
	c.persist()
	c.waitReady("blue")
}
func (c *controller) initMain(o *options) {
	if c.state != nil {
		c.assertDatabase()
		if str(c.state["initial_image"]) != "" && str(c.state["active"]) == "" {
			need(o.maintenance, "interrupted initial conversion requires --maintenance")
			c.resumeInitial()
			return
		}
		c.progress("✓", "已有安装；保留配置和数据，请使用 upgrade/status/resume")
		return
	}
	need(o.maintenance, "首次转换需要 --maintenance；16002 将暂时不可用")
	app, db := c.inspect("container", o.app), c.inspect("container", o.database)
	need(yes(obj(db["State"])["Running"]), "production database must be running")
	settings := env(app)
	dsn, e := url.Parse(str(settings["DATABASE_URL"]))
	check(e, "invalid application PostgreSQL URL")
	password, hasPassword := dsn.User.Password()
	need((dsn.Scheme == "postgres" || dsn.Scheme == "postgresql") && dsn.Hostname() != "" && hasPassword && password != "", "application must contain complete PostgreSQL URL")
	shared := []string{}
	for name := range networks(app) {
		if _, ok := networks(db)[name]; ok {
			shared = append(shared, name)
		}
	}
	network := o.databaseNetwork
	if network == "" && len(shared) == 1 {
		network = shared[0]
	}
	matched := false
	for _, name := range shared {
		matched = matched || name == network
	}
	need(matched, "specify the shared --database-network")
	bindings := list(obj(obj(app["NetworkSettings"])["Ports"])["8080/tcp"])
	need(len(bindings) == 1 && str(obj(bindings[0])["HostPort"]) == fmt.Sprint(o.port), "existing app must own requested entrance port")
	for _, m := range list(app["Mounts"]) {
		need(str(obj(m)["Destination"]) == "/app/data", "additional application mounts need explicit migration review")
	}
	image, proxy := c.imageID(o.image), c.imageID(o.proxyImage)
	c.bundle(image, func(bundle string) {
		contract := load(filepath.Join(bundle, "release.json"))
		contractOK(contract, nil)
		c.memory(contract)
		prefix := fmt.Sprintf("dreamtrans-%x", sha256.Sum256([]byte(c.root)))[:21]
		bind := str(obj(bindings[0])["HostIp"])
		if bind == "" {
			bind = "127.0.0.1"
		}
		port := dsn.Port()
		if port == "" {
			port = "5432"
		}
		c.state = object{"format": 1, "prefix": prefix, "network": prefix + "-entry", "database_network": network, "database_id": db["Id"], "database_image": db["Image"], "database_volume": c.dataMount(db, "/var/lib/postgresql/data"), "application_volume": c.dataMount(app, "/app/data"), "application_env": settings, "legacy_id": app["Id"], "legacy_restart": obj(obj(app["HostConfig"])["RestartPolicy"])["Name"], "proxy_image": proxy, "port": o.port, "bind": bind, "database_env": object{"PGHOST": dsn.Hostname(), "PGPORT": port, "PGDATABASE": strings.TrimPrefix(dsn.Path, "/"), "PGUSER": dsn.User.Username(), "PGPASSWORD": password}, "active": nil, "previous": nil, "colors": object{}, "phase": "initializing", "initial_image": image, "initial_contract": contract}
		c.persist()
	})
	c.resumeInitial()
}
func (c *controller) resumeInitial() {
	image := str(c.state["initial_image"])
	contract := obj(c.state["initial_contract"])
	if str(c.state["phase"]) == "initializing" {
		c.bundle(image, func(b string) { c.migrate(b, contract) })
		c.ensureEntryNetwork()
		c.progress("维护", "停止旧写入，最终导入；原卷保持原样")
		c.docker("update", "--restart=no", str(c.state["legacy_id"]))
		c.docker("stop", "--timeout", "-1", str(c.state["legacy_id"]))
		c.state["phase"] = "importing"
		c.persist()
	}
	file := filepath.Join(c.path, "import.env")
	atomic(file, envBytes(obj(c.state["application_env"])), 0o600)
	c.docker("run", "--rm", "--network", str(c.state["database_network"]), "--env-file", file, "--mount", "type=volume,src="+str(c.state["application_volume"])+",dst=/app/data", "--entrypoint", "/app/server", image, "deploy-import")
	c.initialColor(image, contract)
	c.control("blue", "active")
	c.ensureProxy("blue")
	c.state["active"] = "blue"
	c.state["phase"] = "ready"
	c.persist()
	c.progress("✓", "固定入口就绪；YuAction 使用 http://dreamtrans:8080")
}
func (c *controller) ensureEntryNetwork() {
	network := str(c.state["network"])
	for _, n := range strings.Split(c.docker("network", "ls", "--format", "{{.Name}}"), "\n") {
		if n == network {
			need(str(obj(c.inspect("network", network)["Labels"])["dreamtrans.release"]) == str(c.state["prefix"]), "entry network belongs to another installation")
			return
		}
	}
	c.docker("network", "create", "--label", "dreamtrans.release="+str(c.state["prefix"]), network)
}
func (c *controller) deploy(o *options) {
	c.assertDatabase()
	phase := str(c.state["phase"])
	need(phase == "ready" || phase == "draining", "unfinished release; use resume/abort/rollback")
	c.syncEntry()
	ref := o.image
	if ref == "" {
		need(!c.edge(), "Edge upgrades require a main-site authorized immutable image")
		ref = c.latest()
	}
	image := c.imageID(ref)
	active := str(c.state["active"])
	colors := obj(c.state["colors"])
	if image == str(obj(colors[active])["image"]) && c.environmentMatches(active) {
		c.progress("✓", "已是该固定版本")
		if phase == "draining" {
			c.finishRelease(o)
		}
		return
	}
	color := "blue"
	if active == "blue" {
		color = "green"
	}
	var contract object
	c.bundle(image, func(bundle string) {
		contract = load(filepath.Join(bundle, "release.json"))
		contractOK(contract, obj(obj(colors[active])["contract"]))
		if !c.edge() && str(obj(c.state["application_env"])["EDGE_ROUTING_ENABLED"]) != "" {
			need(number(contract["edge_configuration"]) >= 1, "candidate cannot preserve separate Edge management/routing settings; use a compatible release")
		}
		c.memory(contract)
		c.migrate(bundle, contract)
	})
	c.state["resolved_image"] = ref
	c.persist()
	c.startColor(color, image, contract)
	if o.pause {
		c.progress("暂停", color+" 已验证，执行 resume 切换")
		return
	}
	c.switchColor(color)
	c.observe(o.observe)
	c.finishRelease(o)
}
func (c *controller) observe(seconds int) {
	c.progress("6/8", fmt.Sprintf("新请求已切换；观察 %ds，旧连接继续运行", seconds))
	err := attempt(func() {
		until := time.Now().Add(time.Duration(seconds) * time.Second)
		for time.Now().Before(until) {
			c.probe(str(c.state["active"]))
			need(c.routeColor() == str(c.state["active"]), "route changed during observation")
			c.sleep(2 * time.Second)
		}
	})
	if err != nil {
		c.rollback()
		fail("observation failed; compatible previous image restored; database writes retained")
	}
	c.state["phase"] = "draining"
	c.persist()
}
func (c *controller) drain(seconds int) {
	old := str(c.state["previous"])
	if old == "" {
		return
	}
	need(old != str(c.state["active"]), "refusing to drain the active version")
	release := obj(obj(c.state["colors"])[old])
	if !yes(obj(c.inspect("container", c.name(old))["State"])["Running"]) {
		need(!c.edge() || yes(release["empty_spool"]), "stopped Edge journal has not been acknowledged")
		if c.yuaction() && c.containerExists(old+"-frontend") {
			c.docker("stop", "--timeout", "-1", c.name(old+"-frontend"))
		}
		c.state["phase"] = "ready"
		c.persist()
		return
	}
	until := time.Now().Add(time.Duration(seconds) * time.Second)
	for {
		s := c.control(old, "draining")
		c.progress("7/8", fmt.Sprintf("%s 排空：WebSocket=%d HTTP=%d 任务=%d", old, number(s["websockets"]), number(s["requests"]), number(s["tasks"])))
		if yes(s["drained"]) {
			if c.edge() {
				release["empty_spool"] = true
				c.persist()
			}
			c.docker("stop", "--timeout", "-1", c.name(old))
			if c.yuaction() {
				c.docker("stop", "--timeout", "-1", c.name(old+"-frontend"))
			}
			c.state["phase"] = "ready"
			c.persist()
			c.progress("8/8", "发布完成；旧镜像保留用于兼容回切")
			return
		}
		if c.yuaction() {
			// Retry offers for streams admitted just before cutover and for a
			// client whose earlier preflight failed. Never force-close either.
			_ = attempt(func() {
				active := str(c.state["active"])
				c.probe(active)
				need(c.routeColor() == active, "YuAction replacement route not confirmed")
				c.control(old, "handoff")
			})
		}
		if !time.Now().Before(until) {
			c.state["phase"] = "draining"
			c.persist()
			if yes(c.drainPolicy()["enabled"]) {
				c.progress("待排空", "保留旧实例与现有转录；后台自动继续检查")
			} else {
				c.progress("待排空", "超时保留旧实例；稍后运行 drain，不强制终止转录")
			}
			return
		}
		c.sleep(3 * time.Second)
	}
}
func (c *controller) rollback() {
	old := str(c.state["previous"])
	need(old != "", "no compatible previous managed release")
	c.assertDatabase()
	colors := obj(c.state["colors"])
	contractOK(obj(obj(colors[old])["contract"]), obj(obj(colors[str(c.state["active"])])["contract"]))
	if !yes(obj(c.inspect("container", c.name(old))["State"])["Running"]) {
		c.docker("start", c.name(old))
	}
	if c.yuaction() {
		c.ensureYuActionFrontend(old)
	}
	c.waitReady(old)
	c.switchColor(old)
	c.state["phase"] = "draining"
	c.persist()
	c.progress("回切", "旧镜像接流；数据库新写入保留；另一实例等待排空")
}
func (c *controller) abort() {
	target := str(c.state["target"])
	need(str(c.state["phase"]) == "candidate" && target != str(c.state["active"]), "abort is only valid before cutover")
	present := c.containerExists(target)
	state := object{}
	if present {
		info := c.inspect("container", c.name(target))
		need(str(info["Image"]) == str(obj(obj(c.state["colors"])[target])["image"]), "candidate image differs from persisted intent")
		state = obj(info["State"])
	}
	running := yes(state["Running"]) && !yes(state["Restarting"])
	if running {
		if err := attempt(func() {
			need(yes(c.control(target, "draining")["drained"]), "candidate still owns work; drain before abort")
		}); err != nil {
			state = obj(c.inspect("container", c.name(target))["State"])
			need(!yes(state["Running"]) || yes(state["Restarting"]), err.Error())
			running = false
		}
	}
	if !running {
		if present {
			c.docker("stop", "--timeout", "-1", c.name(target))
		}
		if c.edge() {
			c.verifyStoppedSpool(target)
		}
	}
	if c.edge() {
		obj(obj(c.state["colors"])[target])["empty_spool"] = true
	}
	if running {
		c.docker("stop", "--timeout", "-1", c.name(target))
	}
	if c.yuaction() && c.containerExists(target+"-frontend") {
		c.docker("stop", "--timeout", "-1", c.name(target+"-frontend"))
	}
	if str(c.state["previous"]) == target {
		c.state["previous"] = nil
	}
	c.state["phase"] = "ready"
	c.state["target"] = nil
	if settings, ok := obj(obj(c.state["colors"])[str(c.state["active"])])["application_env"]; ok && !c.edge() {
		c.state["application_env"] = cloneObject(obj(settings))
	}
	c.persist()
	c.progress("中止", "候选已停止；原版本与新写入保留")
}
func (c *controller) resume(o *options) {
	c.assertDatabase()
	phase := str(c.state["phase"])
	need(phase != "initializing" && phase != "importing", "initial conversion interrupted; rerun init --maintenance or edge install")
	switch phase {
	case "candidate":
		c.recoverCandidate()
		c.switchColor(str(c.state["target"]))
	case "switching":
		c.switchColor(str(c.state["target"]))
	}
	if str(c.state["phase"]) == "observing" {
		c.observe(o.observe)
	}
	if str(c.state["phase"]) == "draining" {
		c.finishRelease(o)
	}
}
func (c *controller) recoverCandidate() {
	if c.yuaction() {
		c.recoverYuActionCandidate()
		return
	}
	color := str(c.state["target"])
	need(color != str(c.state["active"]) && (color == "blue" || color == "green"), "invalid candidate identity")
	release := obj(obj(c.state["colors"])[color])
	if !c.containerExists(color) {
		if c.edge() {
			c.verifyStoppedSpool(color)
		}
		c.startColor(color, str(release["image"]), obj(release["contract"]))
		return
	}
	info := c.inspect("container", c.name(color))
	need(str(info["Image"]) == str(release["image"]), "candidate image differs from persisted intent")
	if c.edge() {
		need(hasMount(info, "/spool", filepath.Join(c.path, color, "spool")), "candidate journal differs from intent")
	} else {
		need(c.dataMount(info, "/app/data") == str(c.state["application_volume"]), "candidate application volume differs from intent")
	}
	if !yes(obj(info["State"])["Running"]) {
		c.docker("start", c.name(color))
	}
	if _, attached := networks(info)[str(c.state["network"])]; !attached {
		c.docker("network", "connect", str(c.state["network"]), c.name(color))
	}
	c.smoke(color)
}
func (c *controller) status() object {
	if c.state == nil {
		return object{"initialized": false}
	}
	s := object{}
	for _, k := range []string{"phase", "active", "previous", "target", "network", "port", "database_volume", "application_volume", "drain_started_at", "handoff_status"} {
		s[k] = c.state[k]
	}
	colors := object{}
	for color, v := range obj(c.state["colors"]) {
		info := object{"image": obj(v)["image"]}
		if c.yuaction() {
			info["frontend_image"] = obj(v)["frontend_image"]
		}
		if attempt(func() {
			for k, v := range c.control(color, "status") {
				info[k] = v
			}
		}) != nil {
			info["mode"] = "stopped/unreachable"
		}
		colors[color] = info
	}
	s["colors"] = colors
	s["drain_policy"] = c.drainPolicy()
	if !c.edge() {
		s["configuration_pending"] = !c.environmentMatches(str(c.state["active"]))
	}
	return s
}

func cloneObject(source object) object {
	result := object{}
	for k, v := range source {
		result[k] = v
	}
	return result
}
func (c *controller) environmentMatches(color string) bool {
	if c.edge() {
		return true
	}
	settings, recorded := obj(obj(c.state["colors"])[color])["application_env"]
	return !recorded || reflect.DeepEqual(obj(settings), obj(c.state["application_env"]))
}
