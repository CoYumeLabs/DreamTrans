// Package ops implements the host-local production lifecycle controller.
// State and locks remain compatible with the original deployment controller.
package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/edgehttp"
	"golang.org/x/sys/unix"
)

type object = map[string]any

// stop unwinds a command through its cleanup defers. Only this private type is
// recovered at the CLI boundary; programming panics are never reported as success.
type stop struct{ message string }

func fail(message string) { panic(stop{message}) }
func need(ok bool, message string) {
	if !ok {
		fail(message)
	}
}
func check(err error, message string) {
	if err != nil {
		fail(message)
	}
}
func str(v any) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	need(ok, "invalid string in deployment state")
	return s
}
func obj(v any) object {
	if v == nil {
		return object{}
	}
	m, ok := v.(map[string]any)
	need(ok, "invalid object in deployment state")
	return m
}
func list(v any) []any {
	if v == nil {
		return nil
	}
	a, ok := v.([]any)
	need(ok, "invalid list in deployment state")
	return a
}
func number(v any) int {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case json.Number:
		x, e := strconv.Atoi(string(n))
		check(e, "invalid number in state")
		return x
	case float64:
		return int(n)
	default:
		fail("invalid number in state")
		return 0
	}
}
func yes(v any) bool { b, _ := v.(bool); return b }
func keys(m object) []string {
	k := make([]string, 0, len(m))
	for name := range m {
		k = append(k, name)
	}
	sort.Strings(k)
	return k
}
func contains(v any, s string) bool {
	for _, x := range list(v) {
		if str(x) == s {
			return true
		}
	}
	return false
}
func decode(data []byte) any {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	check(d.Decode(&v), "invalid JSON response or state")
	return v
}
func load(path string) object {
	//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
	b, e := os.ReadFile(path)
	check(e, "cannot read protected state/configuration file")
	return obj(decode(b))
}
func marshal(v any) []byte {
	b, e := json.MarshalIndent(v, "", "  ")
	check(e, "cannot encode deployment state")
	return append(b, '\n')
}
func exists(path string) bool { _, e := os.Stat(path); return e == nil }
func mkdir(path string)       { check(os.MkdirAll(path, 0o700), "cannot create private lifecycle directory") }
func atomic(path string, b []byte, mode os.FileMode) {
	mkdir(filepath.Dir(path))
	f, e := os.CreateTemp(filepath.Dir(path), ".ops-")
	check(e, "cannot create atomic file")
	tmp := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(tmp) }()
	check(f.Chmod(mode), "cannot protect atomic file")
	_, e = f.Write(b)
	check(e, "cannot write atomic file")
	check(f.Sync(), "cannot sync atomic file")
	check(f.Close(), "cannot close atomic file")
	check(os.Rename(tmp, path), "cannot publish atomic file")
	d, e := os.Open(filepath.Dir(path))
	check(e, "cannot open directory")
	defer func() { _ = d.Close() }()
	check(d.Sync(), "cannot sync directory")
}
func save(path string, v any) { atomic(path, marshal(v), 0o600) }
func hashFile(path string) string {
	//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
	f, e := os.Open(path)
	check(e, "cannot read file for checksum")
	defer func() { _ = f.Close() }()
	h := sha256.New()
	_, e = io.Copy(h, f)
	check(e, "cannot hash file")
	return hex.EncodeToString(h.Sum(nil))
}
func copyFile(src, dst string, mode os.FileMode) {
	//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
	b, e := os.ReadFile(src)
	check(e, "cannot read release asset")
	atomic(dst, b, mode)
}
func lock(path string) func() {
	mkdir(filepath.Dir(path))
	//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	check(e, "cannot open lifecycle lock")
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		_ = f.Close()
		fail("another lifecycle command or spool owner holds the lock")
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }
}
func attempt(fn func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if s, ok := p.(stop); ok {
				err = fmt.Errorf("%s", s.message)
			} else {
				panic(p)
			}
		}
	}()
	fn()
	return nil
}

type runner func(context.Context, []string, string) (string, error)

func execute(ctx context.Context, args []string, input string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // Host-local CLI uses explicit argument arrays, never interpolated shell commands.
	cmd.Stdin = strings.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s operation failed; inspect the service locally (arguments and engine errors withheld)", filepath.Base(args[0]))
	}
	return strings.TrimSpace(out.String()), nil
}

type controller struct {
	httpClient  *http.Client
	ctx         context.Context
	root, path  string
	state       object
	run         runner
	out, errOut io.Writer
	sleep       func(time.Duration)
}

func newController(ctx context.Context, root string, out, errOut io.Writer) *controller {
	path, e := filepath.Abs(root)
	check(e, "invalid installation directory")
	if resolved, e := filepath.EvalSymlinks(path); e == nil {
		path = resolved
	}
	c := &controller{ctx: ctx, root: path, path: filepath.Join(path, ".bluegreen"), run: execute, out: out, errOut: errOut}
	c.httpClient = edgehttp.NewClient(15 * time.Second)
	c.sleep = func(d time.Duration) {
		select {
		case <-ctx.Done():
			fail("operation interrupted; persisted state retained for resume")
		case <-time.After(d):
		}
	}
	mkdir(c.path)
	//nolint:gosec // A private directory needs owner traversal permission.
	check(os.Chmod(c.path, 0o700), "cannot protect state directory")
	if exists(filepath.Join(c.path, "state.json")) {
		c.state = load(filepath.Join(c.path, "state.json"))
		need(number(c.state["format"]) == 1, "unsupported state format; conversion required")
	}
	return c
}
func (c *controller) command(input string, args ...string) string {
	s, e := c.run(c.ctx, args, input)
	check(e, "operation failed: "+filepath.Base(args[0])+" (configuration and credentials withheld)")
	return s
}
func (c *controller) docker(args ...string) string {
	return c.command("", append([]string{"docker"}, args...)...)
}
func (c *controller) inspect(kind, name string) object {
	a := list(decode([]byte(c.docker(kind, "inspect", name))))
	need(len(a) == 1, "ambiguous Docker identity")
	return obj(a[0])
}
func (c *controller) persist()                 { save(filepath.Join(c.path, "state.json"), c.state) }
func (c *controller) name(color string) string { return str(c.state["prefix"]) + "-" + color }
func (c *controller) edge() bool               { return str(c.state["role"]) == "edge" }
func (c *controller) progress(step, message string) {
	bar := ""
	var n, total int
	if _, e := fmt.Sscanf(step, "%d/%d", &n, &total); e == nil && total > 0 {
		done := min(16, max(0, n*16/total))
		bar = " [" + strings.Repeat("#", done) + strings.Repeat("-", 16-done) + "]"
	}
	_, _ = fmt.Fprintf(c.errOut, "[%s]%s %s\n", step, bar, message)
}
func (c *controller) containerExists(color string) bool {
	for _, name := range strings.Split(c.docker("container", "ls", "-a", "--format", "{{.Names}}"), "\n") {
		if name == c.name(color) {
			return true
		}
	}
	return false
}
func networks(info object) object { return obj(obj(info["NetworkSettings"])["Networks"]) }
func env(info object) object {
	m := object{}
	for _, v := range list(obj(info["Config"])["Env"]) {
		k, value, ok := strings.Cut(str(v), "=")
		if ok {
			m[k] = value
		}
	}
	return m
}
func envBytes(m object) []byte {
	var b strings.Builder
	for _, k := range keys(m) {
		v := str(m[k])
		need(!strings.ContainsAny(k+v, "\r\n") && !strings.Contains(k, "="), "environment contains unsupported newline or key")
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return []byte(b.String())
}
func envArgs(m object) []string {
	a := make([]string, 0, 2*len(m))
	for _, k := range keys(m) {
		a = append(a, "-e", k+"="+str(m[k]))
	}
	return a
}
func (c *controller) dataMount(info object, target string) string {
	matches := []object{}
	for _, v := range list(info["Mounts"]) {
		m := obj(v)
		if str(m["Destination"]) == target {
			matches = append(matches, m)
		}
	}
	need(len(matches) == 1, "expected exactly one persistence mount")
	m := matches[0]
	need(str(m["Type"]) == "volume" && yes(m["RW"]), "persistence must use an existing writable named volume")
	v := c.inspect("volume", str(m["Name"]))
	need(str(v["Driver"]) == "local" && len(obj(v["Options"])) == 0, "only ordinary local Docker volumes are supported")
	return str(m["Name"])
}
func (c *controller) assertDatabase() {
	if c.edge() {
		need(exists(filepath.Join(c.root, "config", "edge.json")), "existing Edge identity is missing")
		return
	}
	d := c.inspect("container", str(c.state["database_id"]))
	need(str(d["Id"]) == str(c.state["database_id"]) && c.dataMount(d, "/var/lib/postgresql/data") == str(c.state["database_volume"]), "production database identity or volume changed")
	need(yes(obj(d["State"])["Running"]), "existing database is not running; it will not be recreated")
	c.inspect("volume", str(c.state["application_volume"]))
}

var immutable = regexp.MustCompile(`^(?:[\w./:-]+@)?sha256:[0-9a-f]{64}$`)

func (c *controller) imageID(ref string) string {
	need(immutable.MatchString(ref), "use an immutable repository@sha256:digest or local image ID")
	if strings.Contains(ref, "@") {
		c.progress("1/8", "拉取固定版本镜像")
		c.docker("pull", ref)
	}
	return str(c.inspect("image", ref)["Id"])
}
func (c *controller) bundle(image string, fn func(string)) {
	name := c.docker("create", image)
	defer func() { _ = attempt(func() { c.docker("rm", "-v", name) }) }()
	directory, e := os.MkdirTemp("", "dreamtrans-release-")
	check(e, "cannot create release directory")
	defer func() { _ = os.RemoveAll(directory) }()
	c.docker("cp", name+":/usr/share/dreamtrans/.", directory)
	fn(directory)
}
func contractOK(next, old object) {
	need(number(next["protocol"]) == 1 && number(next["state_epoch"]) == 1, "unsupported release/state protocol")
	need(next["expand_migrations"] != nil, "release lacks reviewed expand-only migration manifest")
	_ = list(next["expand_migrations"])
	if old != nil {
		need(number(next["provider_credentials"]) >= number(old["provider_credentials"]), "candidate cannot preserve central provider credentials; use a compatible release")
		need(number(old["state_epoch"]) == number(next["state_epoch"]), "data epochs are not rollback compatible")
		lo := func(v any) int {
			if v == nil {
				return 1
			}
			return number(v)
		}
		need(lo(next["edge_protocol_min"]) <= lo(old["edge_protocol_min"]) && lo(next["edge_protocol_max"]) >= lo(old["edge_protocol_max"]), "candidate cannot serve protocols already authorized")
	}
}
func (c *controller) memory(contract object) {
	data, e := os.ReadFile("/proc/meminfo")
	check(e, "cannot check free memory")
	available := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			n, e := strconv.Atoi(fields[1])
			check(e, "invalid available memory")
			available = n / 1024
		}
	}
	required := max(128, number(contract["minimum_free_memory_mb"]))
	need(available >= required, fmt.Sprintf("可用内存 %d MiB < %d MiB；保留当前服务", available, required))
	c.progress("2/8", fmt.Sprintf("内存检查 %d MiB 可用，需要 %d MiB", available, required))
}
func (c *controller) control(color, action string) object {
	return obj(decode([]byte(c.docker("exec", c.name(color), "/app/server", "deploy-control", action))))
}
func (c *controller) probe(color string) {
	need(yes(obj(c.inspect("container", c.name(color))["State"])["Running"]), "candidate exited")
	c.docker("exec", c.name(color), "wget", "-qO-", "http://127.0.0.1:8080/readyz")
	need(number(c.control(color, "status")["protocol"]) == 1, "control protocol mismatch")
}
func (c *controller) waitReady(color string) {
	for range 60 {
		if attempt(func() { c.probe(color) }) == nil {
			return
		}
		c.sleep(time.Second)
	}
	fail("instance readiness failed; persisted release remains recoverable")
}
func (c *controller) pg(sql string) string {
	a := append([]string{"docker", "exec", "-i"}, envArgs(obj(c.state["database_env"]))...)
	a = append(a, str(c.state["database_id"]), "psql", "-XAt", "-v", "ON_ERROR_STOP=1")
	return c.command(sql, a...)
}
