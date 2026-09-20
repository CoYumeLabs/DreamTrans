package ops

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // Existing Edge-owned durable queue; never the main-site rag.db.
)

func (c *controller) edgeConfig() object { return load(filepath.Join(c.root, "config", "edge.json")) }
func secureOrigin(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}
func (c *controller) callRaw(config object, path string, payload []byte) object {
	base := str(config["main_url"])
	need(secureOrigin(base), "main URL must be an HTTPS origin")
	req, e := http.NewRequestWithContext(c.ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api/edge-control/"+path, bytes.NewReader(payload))
	check(e, "cannot construct main-site request")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Edge "+str(config["identity"]))
	req.Header.Set("User-Agent", "DreamTrans-Edge/1.0")
	client := &http.Client{Timeout: 15 * time.Second}
	if c.httpClient != nil {
		clone := *c.httpClient
		client = &clone
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, e := client.Do(req)
	check(e, "main HTTPS unavailable; request credentials withheld")
	defer func() { _ = response.Body.Close() }()
	need(response.StatusCode >= 200 && response.StatusCode < 300, fmt.Sprintf("main request rejected, HTTP %d", response.StatusCode))
	if date := response.Header.Get("Date"); date != "" {
		at, e := http.ParseTime(date)
		check(e, "invalid main server clock")
		skew := time.Since(at)
		need(skew < 10*time.Second && skew > -10*time.Second, "本机与主站时钟相差超过 10 秒，请同步时钟")
	}
	b, e := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	check(e, "cannot read main-site response")
	return obj(decode(b))
}
func (c *controller) call(config object, path string, payload object) object {
	return c.callRaw(config, path, marshal(payload))
}
func (c *controller) secret(path, prompt string) string {
	if path != "" {
		f, e := os.Stat(path)
		check(e, "credential file missing")
		need(f.Mode().IsRegular() && f.Mode().Perm()&0o077 == 0, "credential file must be private (chmod 600)")
		//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
		b, e := os.ReadFile(path)
		check(e, "cannot read credential file")
		s := strings.TrimSpace(string(b))
		need(s != "", "credential file is empty")
		return s
	}
	tty, e := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	check(e, "no terminal; supply a protected credential file")
	defer func() { _ = tty.Close() }()
	term, e := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	check(e, "cannot hide credential input")
	original := *term
	term.Lflag &^= unix.ECHO
	check(unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, term), "cannot disable echo")
	defer func() { _ = unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, &original); _, _ = fmt.Fprintln(c.errOut) }()
	_, _ = fmt.Fprint(c.errOut, prompt+"（隐藏输入）：")
	type input struct {
		value string
		err   error
	}
	read := make(chan input, 1)
	go func() {
		value, err := bufio.NewReader(tty).ReadString('\n')
		read <- input{value, err}
	}()
	var value string
	select {
	case result := <-read:
		check(result.err, "credential input interrupted")
		value = strings.TrimSpace(result.value)
	case <-c.ctx.Done():
		fail("credential input interrupted")
	}
	need(value != "", "empty credential")
	return value
}
func (c *controller) startEdgeColor(color, image string, contract object) {
	name := c.name(color)
	dir := filepath.Join(c.path, color)
	mkdir(dir)
	colors := obj(c.state["colors"])
	if c.containerExists(color) {
		_, recorded := colors[color]
		need(recorded, "candidate name is already owned by an unrecorded container")
	}
	if _, recorded := colors[color]; recorded && !c.containerExists(color) {
		c.verifyStoppedSpool(color)
	}
	if old, ok := colors[color]; ok && c.containerExists(color) {
		info := c.inspect("container", name)
		need(str(info["Image"]) == str(obj(old)["image"]), "inactive container image differs from recorded installation")
		if yes(obj(info["State"])["Running"]) {
			need(yes(c.control(color, "status")["drained"]), "old color still owns sessions or unacknowledged results")
			c.docker("stop", "--timeout", "-1", name)
		}
		need(yes(obj(old)["empty_spool"]), "stopped spool has not been acknowledged; resume/drain first")
		c.docker("rm", name)
	}
	for _, sub := range []string{"spool", "deployment"} {
		mkdir(filepath.Join(dir, sub))
		check(os.Chown(filepath.Join(dir, sub), 10001, 10001), "cannot assign Edge directory")
	}
	mode := filepath.Join(dir, "deployment", "mode")
	atomic(mode, []byte("standby\n"), 0o600)
	check(os.Chown(mode, 10001, 10001), "cannot assign Edge mode")
	memory := number(contract["container_memory_mb"])
	if memory == 0 {
		memory = 256
	}
	limit := fmt.Sprintf("%dm", max(128, memory))
	colors[color] = object{"image": image, "contract": contract, "empty_spool": false}
	c.state["phase"] = "candidate"
	c.state["target"] = color
	c.persist()
	c.docker("run", "-d", "--name", name, "--restart", "unless-stopped", "--network", str(c.state["network"]), "--memory", limit, "--memory-swap", limit, "--pids-limit", "128", "--mount", "type=bind,src="+filepath.Join(c.root, "config", "edge.json")+",dst=/config/edge.json,readonly", "--mount", "type=bind,src="+filepath.Join(dir, "spool")+",dst=/spool", "--mount", "type=bind,src="+filepath.Join(dir, "deployment")+",dst=/deployment", "-e", "DREAMTRANS_DEPLOYMENT_MODE=standby", "-e", "DREAMTRANS_DEPLOYMENT_STATE=/deployment/mode", "-e", "APP_VERSION="+image, "--log-opt", "max-size=10m", "--log-opt", "max-file=3", image)
	c.progress("4/8", "启动 Edge "+color)
	for range 90 {
		if attempt(func() { c.probe(color) }) == nil {
			c.progress("5/8", color+" 身份与供应商连接检查通过")
			return
		}
		c.sleep(time.Second)
	}
	fail("candidate readiness failed; current route retained")
}
func (c *controller) installEdge(o *options) {
	if c.state != nil {
		need(str(c.state["phase"]) != "uninstalled", "uninstalled audit directory retained; use a new --dir and registration")
		need(c.edge(), "directory is not an Edge installation")
		c.assertDatabase()
		if str(c.state["active"]) == "" {
			c.ensureEntryNetwork()
			c.finishEdgeInstall(o)
		}
		c.installTools(o.backupFile)
		c.progress("✓", "已有节点身份、配置与队列保留")
		return
	}
	need(runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"), "supported hosts: Linux amd64/arm64")
	need(secureOrigin(o.mainURL), "--main must be an HTTPS origin")
	need(o.maximum > 0 && o.port > 0 && o.port < 65536, "invalid Edge capacity or port")
	ln, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", o.port))
	check(e, "entrance port unavailable")
	check(ln.Close(), "cannot release test port")
	image, proxy := c.imageID(o.image), c.imageID(o.proxyImage)
	var contract object
	c.bundle(image, func(b string) {
		contract = load(filepath.Join(b, "release.json"))
		contractOK(contract, nil)
		c.memory(contract)
	})
	configPath := filepath.Join(c.root, "config", "edge.json")
	config := object{"main_url": o.mainURL}
	if exists(configPath) {
		config = load(configPath)
		need(str(config["main_url"]) == o.mainURL && str(config["node_id"]) != "" && str(config["identity"]) != "", "pending registration differs from requested main site")
	} else {
		token := c.secret(o.registrationFile, "一次性注册凭证")
		registration := c.call(config, "register", object{"token": token})
		for k, v := range registration {
			config[k] = v
		}
		need(str(config["node_id"]) != "" && str(config["identity"]) != "", "registration did not return a node identity")
		save(configPath, config)
	}
	if str(config["provider_key"]) == "" {
		config["provider_key"] = c.secret(o.providerKeyFile, "节点独立 Speechmatics Key")
	}
	origins := []any{strings.TrimRight(o.mainURL, "/")}
	if o.origins != "" {
		origins = nil
		for _, origin := range strings.Split(o.origins, ",") {
			need(secureOrigin(origin), "Edge origins must be HTTPS origins")
			origins = append(origins, origin)
		}
	}
	config["origins"] = origins
	config["maximum"] = o.maximum
	config["training"] = o.training
	save(configPath, config)
	check(os.Chown(configPath, 10001, 10001), "cannot assign Edge configuration")
	check(os.Chmod(configPath, 0o400), "cannot protect Edge configuration")
	id := str(config["node_id"])
	need(len(id) >= 8, "invalid node identity")
	prefix := "dreamtrans-edge-" + id[:8]
	c.state = object{"format": 1, "role": "edge", "prefix": prefix, "network": prefix + "-entry", "port": o.port, "bind": "127.0.0.1", "proxy_image": proxy, "active": nil, "previous": nil, "colors": object{}, "phase": "initializing", "initial_image": image, "initial_contract": contract}
	c.persist()
	c.ensureEntryNetwork()
	c.finishEdgeInstall(o)
	c.installTools(o.backupFile)
}
func (c *controller) finishEdgeInstall(o *options) {
	c.initialColor(str(c.state["initial_image"]), obj(c.state["initial_contract"]))
	c.control("blue", "active")
	c.ensureProxy("blue")
	if o.tunnelImage != "" {
		image := c.imageID(o.tunnelImage)
		token := str(c.edgeConfig()["tunnel_token"])
		if token == "" {
			token = c.secret(o.tunnelTokenFile, "节点专用 Tunnel Token")
		}
		file := filepath.Join(c.root, "config", "tunnel.token")
		atomic(file, []byte(token), 0o400)
		check(os.Chown(file, 65532, 65532), "cannot assign Tunnel token")
		if c.containerExists("tunnel") {
			current := c.inspect("container", c.name("tunnel"))
			need(str(current["Image"]) == image, "existing Tunnel image differs; preserve and investigate")
			if !yes(obj(current["State"])["Running"]) {
				c.docker("start", c.name("tunnel"))
			}
		} else {
			c.docker("run", "-d", "--name", c.name("tunnel"), "--restart", "unless-stopped", "--network", str(c.state["network"]), "--mount", "type=bind,src="+file+",dst=/run/tunnel.token,readonly", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", image, "tunnel", "--no-autoupdate", "run", "--token-file", "/run/tunnel.token")
		}
		c.state["tunnel"] = true
	}
	c.state["active"] = "blue"
	c.state["phase"] = "ready"
	c.persist()
	c.progress("✓", "节点已安装；在主站启用前确认独立 Tunnel 指向 http://dreamtrans:8080")
}
func openSpool(path, mode string) *sql.DB {
	u := url.URL{Scheme: "file", Path: path}
	db, e := sql.Open("sqlite", u.String()+"?mode="+mode)
	check(e, "cannot open existing journal")
	db.SetMaxOpenConns(1)
	return db
}
func (c *controller) verifyStoppedSpool(color string) {
	dir := filepath.Join(c.path, color, "spool")
	unlock := lock(filepath.Join(dir, "owner.lock"))
	defer unlock()
	path := filepath.Join(dir, "outbox.db")
	if !exists(path) {
		files, e := os.ReadDir(dir)
		check(e, "cannot inspect spool")
		for _, f := range files {
			need(f.Name() == "owner.lock", "unknown spool files; abort refused")
		}
		return
	}
	db := openSpool(path, "ro")
	defer func() { _ = db.Close() }()
	var count int
	check(db.QueryRowContext(c.ctx, "SELECT count(*) FROM events").Scan(&count), "candidate journal cannot be verified; data retained")
	need(count == 0, "candidate has unacknowledged results; abort refused")
}
func (c *controller) drainNode() {
	c.call(c.edgeConfig(), "self-mode", object{"mode": "draining"})
	for color := range obj(c.state["colors"]) {
		if yes(obj(c.inspect("container", c.name(color))["State"])["Running"]) {
			s := c.control(color, "draining")
			c.progress("排空", fmt.Sprintf("%s: 连接=%d 任务=%d 已排空=%t", color, number(s["websockets"]), number(s["tasks"]), yes(s["drained"])))
		}
	}
	c.progress("排空", "已停止新会话调度；保留现有会话与未回传队列")
}
func (c *controller) uninstall() {
	config := c.edgeConfig()
	c.call(config, "self-mode", object{"mode": "draining"})
	colors := obj(c.state["colors"])
	for color, v := range colors {
		if yes(obj(c.inspect("container", c.name(color))["State"])["Running"]) {
			need(yes(c.control(color, "draining")["drained"]), "sessions/results remain; uninstall refused")
		} else {
			need(yes(obj(v)["empty_spool"]), "stopped journal not verified empty")
		}
	}
	c.call(config, "self-mode", object{"mode": "revoked"})
	unit := "dreamtrans-edge-" + str(c.state["prefix"])
	if exists("/etc/systemd/system/" + unit + ".timer") {
		c.command("", "systemctl", "disable", "--now", unit+".timer")
	}
	names := append(keys(colors), "proxy")
	if yes(c.state["tunnel"]) {
		names = append(names, "tunnel")
	}
	for _, name := range names {
		c.docker("stop", "--timeout", "-1", c.name(name))
		c.docker("rm", c.name(name))
	}
	c.state["phase"] = "uninstalled"
	c.persist()
	c.progress("✓", "身份已吊销，容器已卸载；配置、队列与审计保留")
}
func (c *controller) reconcileSpool(color string, config object) {
	dir := filepath.Join(c.path, color, "spool")
	unlock := lock(filepath.Join(dir, "owner.lock"))
	defer unlock()
	path := filepath.Join(dir, "outbox.db")
	need(exists(path), "retained journal missing")
	db := openSpool(path, "rw")
	defer func() { _ = db.Close() }()
	_, e := db.ExecContext(c.ctx, "PRAGMA synchronous=FULL")
	check(e, "cannot configure durable journal")
	var integrity string
	check(db.QueryRowContext(c.ctx, "PRAGMA integrity_check").Scan(&integrity), "journal integrity check failed")
	need(integrity == "ok", "journal failed integrity check")
	audit := filepath.Join(c.path, "reconciliation-audit")
	mkdir(audit)
	f, e := os.CreateTemp(audit, color+"-*.db")
	check(e, "cannot create audit backup")
	backup := f.Name()
	check(f.Close(), "cannot close audit file")
	_, e = db.ExecContext(c.ctx, "VACUUM INTO ?", backup)
	check(e, "cannot snapshot retained journal")
	c.progress("对账", color+" 审计备份 SHA256="+hashFile(backup))
	rows, e := db.QueryContext(c.ctx, "SELECT session_id,generation,sequence,payload FROM events WHERE blocked=1 ORDER BY session_id,generation,sequence")
	check(e, "cannot read retained events")
	type event struct {
		session              string
		generation, sequence int64
		payload              string
	}
	events := []event{}
	for rows.Next() {
		var v event
		if e = rows.Scan(&v.session, &v.generation, &v.sequence, &v.payload); e != nil {
			_ = rows.Close()
			check(e, "cannot decode retained event")
		}
		events = append(events, v)
	}
	e = rows.Err()
	_ = rows.Close()
	check(e, "cannot finish retained event scan")
	for _, v := range events {
		event := obj(decode([]byte(v.payload)))
		ack := c.callRaw(config, "archive", []byte(v.payload))
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(v.payload)))
		kind := str(ack["disposition"])
		need(yes(ack["archived"]) && (kind == "fenced" || kind == "closed" || kind == "already_committed") && str(ack["session_id"]) == v.session && int64(number(ack["generation"])) == v.generation && int64(number(ack["sequence"])) == v.sequence && str(ack["event_id"]) == str(event["event_id"]) && str(ack["payload_hash"]) == hash, "archive receipt mismatch; retained events untouched")
		func() {
			tx, e := db.BeginTx(c.ctx, nil)
			check(e, "cannot begin journal acknowledgement")
			defer func() { _ = tx.Rollback() }()
			result, e := tx.ExecContext(c.ctx, "DELETE FROM events WHERE session_id=? AND generation=? AND sequence=? AND payload=? AND blocked=1", v.session, v.generation, v.sequence, v.payload)
			check(e, "cannot acknowledge archived event")
			n, e := result.RowsAffected()
			check(e, "cannot verify journal acknowledgement")
			need(n == 1, "journal changed during reconciliation")
			_, e = tx.ExecContext(c.ctx, "DELETE FROM counters WHERE session_id=? AND generation=? AND NOT EXISTS(SELECT 1 FROM events WHERE session_id=? AND generation=?)", v.session, v.generation, v.session, v.generation)
			check(e, "cannot retire acknowledged counter")
			check(tx.Commit(), "cannot commit journal acknowledgement")
		}()
	}
	var count int
	check(db.QueryRowContext(c.ctx, "SELECT count(*) FROM events").Scan(&count), "cannot inspect remaining events")
	c.progress("对账", fmt.Sprintf("%s: 已归档 %d 条，剩余 %d 条", color, len(events), count))
}
func (c *controller) reconcile() {
	phase := str(c.state["phase"])
	need(phase == "ready" || phase == "draining", "reconcile requires a stable route")
	config := c.edgeConfig()
	c.call(config, "self-mode", object{"mode": "draining"})
	restore := obj(c.state["reconciliation_restore"])
	running := []string{}
	for color := range obj(c.state["colors"]) {
		info := c.inspect("container", c.name(color))
		if yes(obj(info["State"])["Running"]) {
			s := c.control(color, "draining")
			need(number(s["requests"]) == 0 && number(s["websockets"]) == 0 && number(s["tasks"]) == 0, "live work remains; retry after it finishes")
			policy := obj(obj(info["HostConfig"])["RestartPolicy"])
			restart := str(policy["Name"])
			if restart == "" {
				restart = "no"
			}
			if restart == "on-failure" && number(policy["MaximumRetryCount"]) > 0 {
				restart += fmt.Sprintf(":%d", number(policy["MaximumRetryCount"]))
			}
			if _, ok := restore[color]; !ok {
				restore[color] = restart
			}
			running = append(running, color)
		}
	}
	c.state["reconciliation_restore"] = restore
	c.persist()
	err := attempt(func() {
		for _, color := range running {
			c.docker("update", "--restart=no", c.name(color))
			c.docker("kill", "--signal=KILL", c.name(color))
		}
		for color := range obj(c.state["colors"]) {
			c.reconcileSpool(color, config)
		}
	})
	for color, restart := range restore {
		if attempt(func() { c.docker("update", "--restart="+str(restart), c.name(color)); c.docker("start", c.name(color)) }) != nil {
			fail("reconciliation retained data; retry reconcile to restore containers")
		}
	}
	delete(c.state, "reconciliation_restore")
	c.persist()
	if err != nil {
		fail(err.Error())
	}
	c.progress("✓", "归档完成；节点保持排空，请在主站确认后恢复调度")
}
func (c *controller) converge(o *options) {
	if yes(c.state["release_paused"]) || len(obj(c.state["reconciliation_restore"])) > 0 {
		return
	}
	phase := str(c.state["phase"])
	if phase != "ready" && phase != "draining" {
		return
	}
	desired := c.call(c.edgeConfig(), "deployment", object{})
	mode := str(desired["mode"])
	if (mode != "enabled" && mode != "draining") || str(desired["image"]) == "" {
		return
	}
	o.image = str(desired["image"])
	image := c.imageID(o.image)
	if image != str(obj(obj(c.state["colors"])[str(c.state["active"])])["image"]) {
		c.deploy(o)
	} else if phase == "draining" {
		c.drain(o.drainTimeout)
	}
}
