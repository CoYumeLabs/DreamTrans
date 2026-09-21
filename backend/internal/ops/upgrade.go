package ops

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const officialRepository = "ghcr.io/coyumelabs/dreamtrans"

func officialRevision(info object) string {
	labels := obj(obj(info["Config"])["Labels"])
	revision := str(labels["org.opencontainers.image.revision"])
	need(regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision) &&
		str(labels["org.opencontainers.image.source"]) == "https://github.com/CoYumeLabs/DreamTrans",
		"upgrade requires official release source/revision metadata; use deploy for custom images")
	return revision
}

func officialDigest(info object) string {
	for _, value := range list(info["RepoDigests"]) {
		ref := str(value)
		if strings.HasPrefix(ref, officialRepository+"@") && immutable.MatchString(ref) {
			return ref
		}
	}
	fail("official release lacks an immutable repository digest")
	return ""
}

// Prepare under the lifecycle lock, then hand off to this exact release's Go
// controller. The callback releases the lock before the child reloads state.
// Extraction lives until the child exits, including during an atomic self-update.
func (c *controller) prepareUpgrade(o *options, handoff func(string, string, string)) {
	c.assertDatabase()
	need(c.state.Phase == "ready" || c.state.Phase == "draining", "unfinished release; use resume/abort/rollback")
	ref := o.image
	if ref == "" {
		ref = c.latest()
	}
	image := c.imageID(ref)
	info := c.inspect("image", image)
	_ = officialRevision(info)
	ref = officialDigest(info)
	c.bundle(image, func(bundle string) {
		contract := load(filepath.Join(bundle, "release.json"))
		contractOK(contract, obj(obj(c.state.Colors[c.state.Active])["contract"]))
		need(number(contract["upgrade_workflow"]) == 1 && number(contract["container_memory_mb"]) == 0, "release does not support coordinated main upgrades; use deploy for older releases")
		binary, backup := filepath.Join(bundle, "dreamtransctl"), filepath.Join(bundle, "backup.sh")
		need(exists(binary) && exists(backup), "release lacks bundled operations tools")
		//nolint:gosec // The verified release executable requires owner execution permission.
		check(os.Chmod(binary, 0o700), "cannot prepare release controller")
		c.progress("工具", "使用已固定镜像中的 Go 运维工具继续升级")
		handoff(binary, backup, ref)
	})
}

func runPreparedUpgrade(ctx context.Context, binary, backup, image string, o *options, out, errOut io.Writer) {
	args := []string{"--dir", o.root, "upgrade-prepared", "--image", image, "--backup-file", backup,
		"--observe", strconv.Itoa(o.observe), "--drain-timeout", strconv.Itoa(o.drainTimeout)}
	if o.pause {
		args = append(args, "--pause")
	}
	//nolint:gosec // Executable is extracted from an operator-authorized immutable release, never a shell command.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, errOut
	check(cmd.Run(), "upgrade did not complete; see progress above and run status (resume/abort for an unfinished release)")
}

func (c *controller) matchingEdge(revision string, mainContract object) string {
	c.progress("Edge", "核对同一提交的 Edge 安装版本；保留现有节点的发布设置")
	ref := officialRepository + ":edge-" + revision
	c.docker("pull", ref)
	info := c.inspect("image", ref)
	need(officialRevision(info) == revision, "Edge release revision differs from the main release")
	digest := officialDigest(info)
	c.bundle(str(info["Id"]), func(bundle string) {
		contract := load(filepath.Join(bundle, "release.json"))
		contractOK(contract, nil)
		need(number(contract["container_memory_mb"]) > 0 && exists(filepath.Join(bundle, "dreamtransctl")), "matching release is not an Edge image with a Go installer")
		need(number(contract["edge_protocol_min"]) <= number(mainContract["edge_protocol_max"]) &&
			number(contract["edge_protocol_max"]) >= number(mainContract["edge_protocol_min"]) &&
			number(contract["provider_credentials"]) >= number(mainContract["provider_credentials"]), "Edge installer is incompatible with the main release")
	})
	return digest
}

// Only new-node installer defaults change. Per-node desired_image and local
// release_paused are independent and are never written by a main-site upgrade.
func (c *controller) upgradePrepared(o *options) {
	c.assertDatabase()
	phase := c.state.Phase
	need(phase == "ready" || phase == "draining", "unfinished release; use resume/abort/rollback")
	image := c.imageID(o.image)
	revision := officialRevision(c.inspect("image", image))
	active := c.state.Active
	release := obj(c.state.Colors[active])
	settings := cloneObject(c.state.ApplicationEnv)
	c.bundle(image, func(bundle string) {
		contract := load(filepath.Join(bundle, "release.json"))
		contractOK(contract, obj(release["contract"]))
		need(number(contract["upgrade_workflow"]) == 1 && number(contract["container_memory_mb"]) == 0, "unsupported coordinated main release")
		if str(settings["EDGE_RELEASE_IMAGE"]) != "" {
			need(number(contract["edge_configuration"]) >= 1, "candidate cannot preserve Edge configuration")
			settings["EDGE_RELEASE_IMAGE"] = c.matchingEdge(revision, contract)
		}
	})
	// An occupied old color must not be overwritten, even if only the installer
	// environment changed. Finish its existing drain and let the operator retry.
	changed := image != str(release["image"]) || str(settings["EDGE_RELEASE_IMAGE"]) != str(c.state.ApplicationEnv["EDGE_RELEASE_IMAGE"]) || !c.environmentMatches(active)
	if phase == "draining" && changed {
		c.finishRelease(o)
		need(c.state.Phase == "ready", "previous release is still draining; retained all sessions, retry upgrade after draining finishes")
	}
	// Install both tools from the verified bundle; callers cannot substitute an
	// unrelated backup helper. This happens before any production configuration write.
	c.bundle(image, func(bundle string) {
		executable, err := os.Executable()
		check(err, "cannot locate current release controller")
		need(hashFile(executable) == hashFile(filepath.Join(bundle, "dreamtransctl")), "prepared upgrade must run the selected release controller")
		c.installTools(filepath.Join(bundle, "backup.sh"))
	})
	if str(settings["EDGE_RELEASE_IMAGE"]) != str(c.state.ApplicationEnv["EDGE_RELEASE_IMAGE"]) {
		previous := cloneObject(c.state.ApplicationEnv)
		if _, recorded := release["application_env"]; !recorded {
			release["application_env"] = previous
		}
		save(filepath.Join(c.path, "configuration.previous.json"), previous)
		c.state.ApplicationEnv = settings
		c.persist()
	}
	c.deploy(o)
	if c.state.Phase != "candidate" {
		c.upgradeYuActionCompanion(o)
	}
	c.progress("状态", "升级状态如下；draining 表示旧会话仍在排空")
	_, _ = c.out.Write(marshal(c.status()))
}

func (c *controller) dispatchPreparedUpgrade(o *options) {
	need(!c.edge() && !c.yuaction(), "prepared upgrade is main-only")
	c.upgradePrepared(o)
}
