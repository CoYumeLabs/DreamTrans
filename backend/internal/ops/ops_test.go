package ops

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testController(t *testing.T) *controller {
	t.Helper()
	c := newController(t.Context(), t.TempDir(), io.Discard, io.Discard)
	contract := object{"protocol": 1, "state_epoch": 1, "edge_protocol_min": 1, "edge_protocol_max": 2, "expand_migrations": []any{}, "minimum_free_memory_mb": 128}
	c.state = object{"format": 1, "prefix": "fixture", "network": "entry", "database_network": "original", "database_id": "db-id", "database_volume": "real-pg", "application_volume": "real-app", "proxy_image": "proxy-id", "phase": "ready", "active": "blue", "previous": "green", "target": "blue", "colors": object{"blue": object{"image": "blue-image", "contract": contract}, "green": object{"image": "green-image", "contract": contract}}}
	c.sleep = func(time.Duration) {}
	return c
}
func testEngine(c *controller) (*[]string, *string, *object) {
	calls := []string{}
	route := "blue"
	control := object{"protocol": 1, "drained": true, "requests": 0, "websockets": 0, "tasks": 0}
	c.run = func(_ context.Context, a []string, _ string) (string, error) {
		calls = append(calls, strings.Join(a, " "))
		if len(a) > 3 && a[2] == "inspect" {
			kind, name := a[1], a[3]
			var info object
			switch kind {
			case "volume":
				info = object{"Name": name, "Driver": "local", "Options": nil}
			case "network":
				info = object{"Containers": object{}, "Labels": object{"dreamtrans.release": "fixture"}}
			case "container":
				info = object{
					"Id": name, "Image": "proxy-id", "State": object{"Running": true},
					"Mounts": []any{
						object{"Type": "volume", "Name": "real-pg", "Destination": "/var/lib/postgresql/data", "RW": true},
						object{"Source": filepath.Join(c.path, "proxy"), "Destination": "/release"},
					},
					"NetworkSettings": object{"Networks": object{
						"entry": object{}, "original": object{"Aliases": []any{"dreamtrans"}},
					}},
				}
			}
			return string(marshal([]any{info})), nil
		}
		if len(a) > 4 && a[1] == "exec" && a[3] == "/app/server" {
			return string(marshal(control)), nil
		}
		if len(a) > 4 && a[1] == "exec" && a[3] == "wget" {
			if strings.HasSuffix(a[len(a)-1], "/_release") {
				return route, nil
			}
			return "ready", nil
		}
		return "", nil
	}
	return &calls, &route, &control
}
func requireFailure(t *testing.T, fn func(), part string) {
	t.Helper()
	e := attempt(fn)
	if e == nil || !strings.Contains(e.Error(), part) {
		t.Fatalf("expected %q, got %v", part, e)
	}
}

func TestStateCompatibilityAndExclusiveLock(t *testing.T) {
	c := testController(t)
	c.state["unknown_future"] = decode([]byte(`{"counter":9007199254740993,"value":"preserve"}`))
	c.persist()
	before := load(filepath.Join(c.path, "state.json"))
	other := newController(t.Context(), c.root, io.Discard, io.Discard)
	other.persist()
	if !reflect.DeepEqual(before, load(filepath.Join(c.path, "state.json"))) {
		t.Fatal("existing state changed")
	}
	unlock := lock(filepath.Join(c.path, "lock"))
	requireFailure(t, func() { lock(filepath.Join(c.path, "lock")) }, "holds the lock")
	unlock()
	unlock = lock(filepath.Join(c.path, "lock"))
	unlock()
	info, e := os.Stat(filepath.Join(c.path, "state.json"))
	if e != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("state not private")
	}
}
func TestDrainRetainsLongRunningWork(t *testing.T) {
	c := testController(t)
	calls, _, control := testEngine(c)
	(*control)["drained"] = false
	(*control)["websockets"] = 1
	c.drain(0)
	if c.state["phase"] != "draining" {
		t.Fatal("lost drain progress")
	}
	for _, call := range *calls {
		if strings.Contains(call, " stop ") {
			t.Fatal("terminated a live transcript")
		}
	}
	(*control)["websockets"] = 0
	(*control)["drained"] = true
	c.drain(0)
	if c.state["phase"] != "ready" {
		t.Fatal("did not complete drain")
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "docker stop --timeout -1 fixture-green") {
		t.Fatal("acknowledged instance not retired")
	}
}

func TestBackgroundDrainDeadlineRecoveryAndCompatibility(t *testing.T) {
	c := testController(t)
	calls, _, control := testEngine(c)
	(*control)["drained"] = false
	(*control)["websockets"] = 1
	c.state["phase"] = "draining"
	save(filepath.Join(c.path, "drain-policy.json"), object{"enabled": true, "handoff_after_seconds": 600})
	c.drainTick() // Adopt a release whose old CLI did not record its deadline.
	started := str(c.state["drain_started_at"])
	if started == "" {
		t.Fatal("deadline not persisted")
	}
	c.drainTick()
	if strings.Contains(strings.Join(*calls, "\n"), "deploy-control handoff") {
		t.Fatal("premature handoff")
	}
	c.state["drain_started_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	c.persist()
	restarted := newController(t.Context(), c.root, io.Discard, io.Discard)
	restarted.run, restarted.sleep = c.run, c.sleep
	restarted.drainTick()
	if restarted.state["handoff_status"] != "old_version_requires_natural_drain" {
		t.Fatal(restarted.state)
	}
	(*control)["handoff_supported"] = true
	restarted.drainTick()
	if !strings.Contains(strings.Join(*calls, "\n"), "deploy-control handoff") {
		t.Fatal("deadline did not offer handoff")
	}
	if strings.Contains(strings.Join(*calls, "\n"), "docker stop") {
		t.Fatal("handoff forcibly killed a live user")
	}
	(*control)["drained"] = true
	restarted.drainTick()
	if restarted.state["phase"] != "ready" {
		t.Fatal("automatic retirement failed")
	}
}

func TestBackgroundDrainDoesNotMigrateToBrokenRoute(t *testing.T) {
	c := testController(t)
	calls, route, control := testEngine(c)
	(*control)["drained"], (*control)["handoff_supported"] = false, true
	c.state["phase"] = "draining"
	c.state["drain_started_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	save(filepath.Join(c.path, "drain-policy.json"), object{"enabled": true, "handoff_after_seconds": 0})
	*route = "green"
	c.drainTick()
	if c.state["handoff_status"] != "handoff_check_failed_retrying" {
		t.Fatal(c.state)
	}
	if strings.Contains(strings.Join(*calls, "\n"), "deploy-control handoff") {
		t.Fatal("offered broken replacement")
	}
	c.state["phase"] = "observing"
	before := len(*calls)
	c.drainTick()
	if len(*calls) != before {
		t.Fatal("timer interfered with foreground observation")
	}
}

func TestDrainTimerIsRebootPersistentAndRoleSpecific(t *testing.T) {
	for _, role := range []string{"main", "edge"} {
		c := testController(t)
		c.state["role"] = role
		dir := t.TempDir()
		c.writeDrainUnits(dir, "test-drain")
		service, _ := os.ReadFile(filepath.Join(dir, "test-drain.service"))
		timer, _ := os.ReadFile(filepath.Join(dir, "test-drain.timer"))
		if !strings.Contains(string(timer), "OnBootSec=20") || !strings.Contains(string(timer), "WantedBy=timers.target") {
			t.Fatal(string(timer))
		}
		if !strings.Contains(string(service), " drain-tick") || strings.Contains(string(service), " edge --dir") != (role == "edge") {
			t.Fatal(string(service))
		}
	}
}
func TestSwitchFailurePreservesRouteAndResumes(t *testing.T) {
	c := testController(t)
	_, route, _ := testEngine(c)
	base := c.run
	reject := true
	c.run = func(ctx context.Context, a []string, in string) (string, error) {
		if strings.Contains(strings.Join(a, " "), "nginx -s reload") {
			if reject {
				reject = false
				return "", fmt.Errorf("fixture failure")
			}
			data, _ := os.ReadFile(filepath.Join(c.path, "proxy", "nginx.conf"))
			if strings.Contains(string(data), `return 200 "green"`) {
				*route = "green"
			} else {
				*route = "blue"
			}
		}
		return base(ctx, a, in)
	}
	requireFailure(t, func() { c.switchColor("green") }, "operation failed")
	if *route != "blue" || c.state["active"] != "blue" || c.state["phase"] != "switching" {
		t.Fatal("failed cutover lost original route or recovery intent")
	}
	c.resume(&options{observe: 0, drainTimeout: 0})
	if *route != "green" || c.state["active"] != "green" || c.state["previous"] != "blue" || c.state["phase"] != "ready" {
		t.Fatal("resume did not complete intended cutover")
	}
	if c.state["database_volume"] != "real-pg" || c.state["application_volume"] != "real-app" {
		t.Fatal("production volumes changed")
	}
}
func TestObservationFailureRollsBackWithoutRestoringDatabase(t *testing.T) {
	c := testController(t)
	c.state["active"] = "green"
	c.state["previous"] = "blue"
	calls, route, _ := testEngine(c)
	*route = "green"
	base := c.run
	c.run = func(ctx context.Context, a []string, in string) (string, error) {
		joined := strings.Join(a, " ")
		if strings.Contains(joined, "exec fixture-green wget") {
			return "", fmt.Errorf("readiness failed")
		}
		if strings.Contains(joined, "nginx -s reload") {
			*route = "blue"
		}
		return base(ctx, a, in)
	}
	requireFailure(t, func() { c.observe(1) }, "previous image restored")
	if c.state["active"] != "blue" || *route != "blue" {
		t.Fatal("did not restore compatible route")
	}
	for _, v := range *calls {
		if strings.Contains(v, "pg_restore") || strings.Contains(v, "volume rm") {
			t.Fatal("rollback touched production data")
		}
	}
}
func TestProtocolDowngradeAndStoppedEdgeSpoolAreRejected(t *testing.T) {
	c := testController(t)
	old := obj(obj(obj(c.state["colors"])["blue"])["contract"])
	candidate := object{"protocol": 1, "state_epoch": 1, "edge_protocol_min": 1, "edge_protocol_max": 1, "expand_migrations": []any{}}
	requireFailure(t, func() { contractOK(candidate, old) }, "already authorized")
	c.state["role"] = "edge"
	dir := filepath.Join(c.path, "green", "spool")
	mkdir(dir)
	c.verifyStoppedSpool("green")
	db, e := sql.Open("sqlite", filepath.Join(dir, "outbox.db"))
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`CREATE TABLE events(payload TEXT); INSERT INTO events VALUES('unsent')`)
	if e != nil {
		t.Fatal(e)
	}
	_ = db.Close()
	requireFailure(t, func() { c.verifyStoppedSpool("green") }, "unacknowledged")
	hold := lock(filepath.Join(dir, "owner.lock"))
	requireFailure(t, func() { c.verifyStoppedSpool("green") }, "holds the lock")
	hold()
	_ = os.WriteFile(filepath.Join(dir, "outbox.db"), []byte("corrupt"), 0o600)
	requireFailure(t, func() { c.verifyStoppedSpool("green") }, "cannot be verified")
}
func TestLatestResolvesImmutableDigestAndRejectsBadMetadata(t *testing.T) {
	c := testController(t)
	digest := "ghcr.io/coyumelabs/dreamtrans@sha256:" + strings.Repeat("a", 64)
	labels := object{"org.opencontainers.image.revision": strings.Repeat("b", 40), "org.opencontainers.image.source": "https://github.com/CoYumeLabs/DreamTrans"}
	calls := []string{}
	c.run = func(_ context.Context, a []string, _ string) (string, error) {
		calls = append(calls, strings.Join(a, " "))
		if a[1] == "pull" {
			return "", nil
		}
		return string(marshal([]any{object{"Config": object{"Labels": labels}, "RepoDigests": []any{digest}}})), nil
	}
	if c.latest() != digest {
		t.Fatal("did not freeze latest to digest")
	}
	labels["org.opencontainers.image.source"] = "https://example.test/other"
	requireFailure(t, func() { c.latest() }, "source metadata")
	for _, call := range calls {
		if strings.Contains(call, " run ") || strings.Contains(call, " stop ") {
			t.Fatal("version discovery changed running services")
		}
	}
}
func TestConfigurationArchiveIncludesSecretsAndExcludesDerivedAssets(t *testing.T) {
	c := testController(t)
	c.persist()
	expected := map[string]string{".env": "main-secret", "compose.production.yml": "real volumes", "yuaction/.env": "linked-secret", "yuaction/.project": "yuaction", "dreamtransctl": "binary-fixture"}
	for name, value := range expected {
		path := filepath.Join(c.root, name)
		mkdir(filepath.Dir(path))
		if name == "yuaction/.env" {
			atomic(filepath.Join(c.root, "secret"), []byte(value), 0o600)
			if e := os.Symlink(filepath.Join(c.root, "secret"), path); e != nil {
				t.Fatal(e)
			}
		} else {
			atomic(path, []byte(value), 0o600)
		}
	}
	atomic(filepath.Join(c.path, "lock"), nil, 0o600)
	atomic(filepath.Join(c.path, "migration", "secret.sql"), []byte("derived"), 0o600)
	archive := filepath.Join(t.TempDir(), "configuration.tar")
	c.archiveConfiguration(archive)
	f, e := os.Open(archive)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	reader := tar.NewReader(f)
	seen := map[string]string{}
	for {
		header, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if header.Typeflag == tar.TypeReg {
			b, e := io.ReadAll(reader)
			if e != nil {
				t.Fatal(e)
			}
			seen[header.Name] = string(b)
		}
	}
	for name, value := range expected {
		if seen[name] != value {
			t.Fatalf("missing configuration %s", name)
		}
	}
	for name := range seen {
		if strings.Contains(name, "migration/") || name == ".bluegreen/lock" || name == "secret" {
			t.Fatalf("unexpected archived file %s", name)
		}
	}
}
func TestReconcileRequiresExactReceiptAndKeepsOriginalJSON(t *testing.T) {
	c := testController(t)
	dir := filepath.Join(c.path, "blue", "spool")
	mkdir(dir)
	path := filepath.Join(dir, "outbox.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	payload := `{"session_id":"session","generation":1,"sequence":2,"event_id":"event","kind":"end","text":"<literal>","duration":1.0}`
	_, e = db.Exec(`CREATE TABLE events(session_id TEXT,generation INTEGER,sequence INTEGER,payload TEXT,blocked INTEGER); CREATE TABLE counters(session_id TEXT,generation INTEGER); INSERT INTO counters VALUES('session',1); INSERT INTO events VALUES('session',1,2,?,1)`, payload)
	if e != nil {
		t.Fatal(e)
	}
	_ = db.Close()
	bad := true
	count := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		b, _ := io.ReadAll(r.Body)
		if string(b) != payload {
			t.Error("reencoded retained payload")
		}
		if r.Header.Get("Authorization") != "Edge node-secret" {
			t.Error("node identity missing")
		}
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
		if bad {
			sum = "bad"
		}
		_, _ = w.Write(marshal(object{"archived": true, "disposition": "fenced", "session_id": "session", "generation": 1, "sequence": 2, "event_id": "event", "payload_hash": sum}))
	}))
	defer server.Close()
	c.httpClient = server.Client()
	config := object{"main_url": server.URL, "identity": "node-secret"}
	requireFailure(t, func() { c.reconcileSpool("blue", config) }, "receipt mismatch")
	db, e = sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	var pending int
	if e = db.QueryRow(`SELECT count(*) FROM events`).Scan(&pending); e != nil || pending != 1 {
		t.Fatal("unconfirmed result was lost")
	}
	_ = db.Close()
	bad = false
	c.reconcileSpool("blue", config)
	c.reconcileSpool("blue", config)
	if count != 2 {
		t.Fatalf("unexpected archive requests %d", count)
	}
	db, e = sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.QueryRow(`SELECT (SELECT count(*) FROM events)+(SELECT count(*) FROM counters)`).Scan(&pending); e != nil || pending != 0 {
		t.Fatal("acknowledged events not retired")
	}
}
func TestNodeRequestsDoNotFollowCredentialRedirects(t *testing.T) {
	c := testController(t)
	leaked := false
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer receiver.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, receiver.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c.httpClient = redirect.Client()
	requireFailure(t, func() { c.call(object{"main_url": redirect.URL, "identity": "private"}, "deployment", object{}) }, "HTTP 307")
	if leaked {
		t.Fatal("identity followed redirect")
	}
}
func TestCLINeedsNoPythonAndPreservesUnknownState(t *testing.T) {
	var out bytes.Buffer
	root := t.TempDir()
	if e := Run(t.Context(), []string{"--dir", root, "status"}, &out, io.Discard); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), `"initialized": false`) {
		t.Fatal(out.String())
	}
	o := parse([]string{"edge", "--image", "image", "--main", "https://main.test"}, io.Discard)
	if o.action != "install" || !o.edge {
		t.Fatal("bootstrap no longer defaults to install")
	}
}

func TestEdgeUninstallRefusesUnacknowledgedOrActiveWork(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			c := testController(t)
			c.state["role"] = "edge"
			calls, _, control := testEngine(c)
			(*control)["drained"] = false
			(*control)["websockets"] = 1
			base := c.run
			c.run = func(ctx context.Context, args []string, input string) (string, error) {
				out, err := base(ctx, args, input)
				if stopped && len(args) > 3 && args[1] == "container" && args[2] == "inspect" {
					items := list(decode([]byte(out)))
					obj(items[0])["State"] = object{"Running": false}
					out = string(marshal(items))
				}
				return out, err
			}
			modes := []string{}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				modes = append(modes, str(obj(decode(body))["mode"]))
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			c.httpClient = server.Client()
			save(filepath.Join(c.root, "config", "edge.json"), object{"main_url": server.URL, "identity": "fixture"})
			if attempt(c.uninstall) == nil {
				t.Fatal("uninstalled with pending results or active work")
			}
			if !reflect.DeepEqual(modes, []string{"draining"}) {
				t.Fatal("revoked a node before confirming its results")
			}
			for _, call := range *calls {
				if strings.Contains(call, " stop ") || strings.Contains(call, " rm ") {
					t.Fatal("removed an unacknowledged journal owner")
				}
			}
		})
	}
}

func TestMainIdentityMismatchBlocksToolReplacement(t *testing.T) {
	c := testController(t)
	testEngine(c)
	c.state["database_volume"] = "unexpected-empty-volume"
	path := filepath.Join(c.root, "dreamtransctl")
	atomic(path, []byte("previous binary"), 0o700)
	requireFailure(t, func() { c.installTools("") }, "identity or volume changed")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "previous binary" {
		t.Fatal("replaced tooling despite mismatched production identity")
	}
}

func TestCLIHelpDoesNotRequireInstallation(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"edge", "--help"}} {
		var output bytes.Buffer
		if err := Run(t.Context(), args, &output, io.Discard); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "Go") {
			t.Fatal("help missing")
		}
	}
}
