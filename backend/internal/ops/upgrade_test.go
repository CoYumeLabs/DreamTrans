package ops

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type upgradeFixture struct {
	c                                   *controller
	calls                               *[]string
	control                             *object
	image, digest, revision, edgeDigest string
	failPull, failTools, interrupted    bool
	edgeRevision                        string
}

// Exercise the real preparation/deployment controller with a deterministic
// Docker boundary. No production Docker daemon or installation is touched.
func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	c := testController(t)
	calls, _, control := testEngine(c)
	fallback := c.run
	f := &upgradeFixture{c: c, calls: calls, control: control, image: "sha256:" + strings.Repeat("a", 64), digest: officialRepository + "@sha256:" + strings.Repeat("b", 64), revision: strings.Repeat("c", 40), edgeDigest: officialRepository + "@sha256:" + strings.Repeat("d", 64)}
	contract := cloneObject(obj(obj(c.state.Colors["blue"])["contract"]))
	contract["upgrade_workflow"] = 1
	contract["provider_credentials"] = 1
	contract["edge_configuration"] = 1
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c.run = func(ctx context.Context, a []string, input string) (string, error) {
		if len(a) > 2 && a[0] == "docker" {
			switch a[1] {
			case "pull":
				*calls = append(*calls, strings.Join(a, " "))
				if f.failPull {
					return "", errors.New("missing image")
				}
				return "", nil
			case "image":
				if len(a) > 3 && a[2] == "inspect" {
					revision, digest, id := f.revision, f.digest, f.image
					if strings.Contains(a[3], ":edge-") {
						digest = f.edgeDigest
						id = "edge-image"
						if f.edgeRevision != "" {
							revision = f.edgeRevision
						}
					}
					return string(marshal([]any{object{"Id": id, "RepoDigests": []any{digest}, "Config": object{"Labels": object{"org.opencontainers.image.revision": revision, "org.opencontainers.image.source": "https://github.com/CoYumeLabs/DreamTrans"}}}})), nil
				}
			case "create":
				return "bundle-" + a[2], nil
			case "cp":
				dir := a[len(a)-1]
				if f.interrupted {
					return "", context.Canceled
				}
				candidate := cloneObject(contract)
				if strings.Contains(a[2], "edge-image") {
					candidate["container_memory_mb"] = 256
				}
				save(filepath.Join(dir, "release.json"), candidate)
				copyFile(executable, filepath.Join(dir, "dreamtransctl"), 0o700)
				if f.failTools {
					atomic(filepath.Join(dir, "dreamtransctl"), []byte("different executable"), 0o700)
				}
				atomic(filepath.Join(dir, "backup.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
				return "", nil
			}
		}
		return fallback(ctx, a, input)
	}
	return f
}

func TestUpgradePreparationPinsBundleAndReleasesForReload(t *testing.T) {
	f := newUpgradeFixture(t)
	f.c.persist()
	unlock := lock(filepath.Join(f.c.path, "lock"))
	defer func() { unlock() }()
	called := false
	f.c.prepareUpgrade(&options{image: f.digest}, func(binary, backup, image string) {
		called = true
		if !exists(binary) || !exists(backup) || image != f.digest {
			t.Fatal("unverified handoff")
		}
		unlock()
		unlock = func() {}
		childUnlock := lock(filepath.Join(f.c.path, "lock"))
		defer childUnlock()
		child := newController(t.Context(), f.c.root, io.Discard, io.Discard)
		if child.state.Active != "blue" || child.state.Phase != "ready" {
			t.Fatal("child did not reload stable state")
		}
	})
	if !called {
		t.Fatal("handoff not called")
	}
}

func TestUpgradeFailureBeforeHandoffRetainsStateAndTools(t *testing.T) {
	for _, mode := range []string{"missing-image", "interrupted-bundle"} {
		t.Run(mode, func(t *testing.T) {
			f := newUpgradeFixture(t)
			f.c.persist()
			before := string(marshal(f.c.state))
			f.failPull = mode == "missing-image"
			f.interrupted = mode == "interrupted-bundle"
			err := attempt(func() {
				f.c.prepareUpgrade(&options{image: f.digest}, func(string, string, string) { t.Fatal("unexpected handoff") })
			})
			if err == nil || string(marshal(f.c.state)) != before || exists(filepath.Join(f.c.root, "dreamtransctl")) {
				t.Fatal("failed prepare changed installation")
			}
			f.failPull = false
			f.interrupted = false
			f.c.prepareUpgrade(&options{image: f.digest}, func(string, string, string) {})
		})
	}
}

func TestUpgradePreparedRejectsOccupiedOldColor(t *testing.T) {
	f := newUpgradeFixture(t)
	f.c.state.Phase = "draining"
	(*f.control)["drained"] = false
	(*f.control)["websockets"] = 1
	f.c.persist()
	requireFailure(t, func() { f.c.upgradePrepared(&options{image: f.digest, drainTimeout: 0}) }, "still draining")
	if f.c.state.Active != "blue" || exists(filepath.Join(f.c.root, "dreamtransctl")) {
		t.Fatal("occupied release changed")
	}
	for _, call := range *f.calls {
		if strings.Contains(call, " stop ") || strings.Contains(call, " rm fixture-") {
			t.Fatal("live color destroyed")
		}
	}
}

func TestUpgradePreparedToolFailureCannotPublishConfiguration(t *testing.T) {
	for _, mode := range []string{"wrong-controller", "backup-write"} {
		t.Run(mode, func(t *testing.T) {
			f := newUpgradeFixture(t)
			f.c.state.ApplicationEnv = object{"EDGE_RELEASE_IMAGE": "old-edge", "EDGE_ROUTING_ENABLED": "false"}
			f.c.persist()
			before := string(marshal(f.c.state))
			if mode == "wrong-controller" {
				f.failTools = true
			} else {
				mkdir(filepath.Join(f.c.root, "backup.sh"))
			}
			err := attempt(func() { f.c.upgradePrepared(&options{image: f.digest}) })
			if err == nil {
				t.Fatal("failed tool replacement accepted")
			}
			if string(marshal(f.c.state)) != before || exists(filepath.Join(f.c.path, "configuration.previous.json")) {
				t.Fatal("tool failure published configuration")
			}
			reloaded := newController(t.Context(), f.c.root, io.Discard, io.Discard)
			if reloaded.state.ApplicationEnv["EDGE_RELEASE_IMAGE"] != "old-edge" {
				t.Fatal("lost installer pin")
			}
		})
	}
}

func TestUpgradePreparedSameReleaseIsIdempotent(t *testing.T) {
	f := newUpgradeFixture(t)
	obj(f.c.state.Colors["blue"])["image"] = f.image
	f.c.persist()
	for range 2 {
		f.c.upgradePrepared(&options{image: f.digest})
	}
	if f.c.state.Active != "blue" || f.c.state.Phase != "ready" {
		t.Fatal("identical upgrade switched colors")
	}
	if !exists(filepath.Join(f.c.root, "dreamtransctl")) || !exists(filepath.Join(f.c.root, "backup.sh")) {
		t.Fatal("tools not installed")
	}
	for _, call := range *f.calls {
		if strings.Contains(call, " stop ") || strings.Contains(call, "--name fixture-green") {
			t.Fatal("idempotent upgrade restarted application")
		}
	}
}

func TestMatchingEdgeRequiresSameCommitAndCapability(t *testing.T) {
	f := newUpgradeFixture(t)
	contract := object{"edge_protocol_min": 1, "edge_protocol_max": 2, "provider_credentials": 1}
	if got := f.c.matchingEdge(f.revision, contract); got != f.edgeDigest {
		t.Fatalf("digest=%s", got)
	}
	f.edgeRevision = strings.Repeat("e", 40)
	requireFailure(t, func() { f.c.matchingEdge(f.revision, contract) }, "revision differs")
	f.edgeRevision = ""
	contract["provider_credentials"] = 2
	requireFailure(t, func() { f.c.matchingEdge(f.revision, contract) }, "incompatible")
}

func TestPreparedUpgradeProcessKeepsArgumentsAndCancellation(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "release controller")
	atomic(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.args\"\n"), 0o700)
	options := &options{root: "/installation with spaces", observe: 17, drainTimeout: 23, pause: true}
	runPreparedUpgrade(t.Context(), binary, "/backup with spaces", "pinned-image", options, io.Discard, io.Discard)
	got, err := os.ReadFile(binary + ".args")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dir", options.root, "upgrade-prepared", "--image", "pinned-image", "--backup-file", "/backup with spaces", "--observe", "17", "--drain-timeout", "23", "--pause"}
	if string(got) != strings.Join(want, "\n")+"\n" {
		t.Fatalf("handoff arguments=%q", got)
	}
	if err := os.Remove(binary + ".args"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requireFailure(t, func() { runPreparedUpgrade(ctx, binary, "backup", "image", options, io.Discard, io.Discard) }, "upgrade did not complete")
	if exists(binary + ".args") {
		t.Fatal("cancelled handoff ran the child")
	}
}

func TestYuActionInitialContractBeforeStateExists(t *testing.T) {
	c := newController(t.Context(), t.TempDir(), io.Discard, io.Discard)
	c.run = func(_ context.Context, args []string, _ string) (string, error) {
		switch args[1] {
		case "create":
			return "fixture", nil
		case "cp":
			dir := args[3]
			save(filepath.Join(dir, "release.json"), object{"protocol": 1, "state_epoch": 1, "component": "yuaction", "recording_handoff": 1, "expand_migrations": []any{"001.sql"}})
			atomic(filepath.Join(dir, "migrations", "001.sql"), []byte("SELECT 1;"), 0o600)
		}
		return "", nil
	}
	contract := c.yuactionContract("fixture-image")
	if c.state != nil || str(obj(contract["schema"])["001.sql"]) == "" {
		t.Fatal("initial release did not validate before state creation")
	}
}
