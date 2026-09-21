package ops

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (c *controller) drainPolicy() object {
	path := filepath.Join(c.path, "drain-policy.json")
	if !exists(path) {
		return object{}
	}
	return load(path)
}

// Timer invocations are short and use the same exclusive lifecycle lock as
// foreground commands. No shell job, SSH session or in-memory deadline owns a
// release; after reboot the persisted timestamp remains authoritative.
func (c *controller) configureDrain(seconds int) {
	need(seconds >= -1 && seconds <= 604800, "handoff-after must be -1 (never) or 0..604800 seconds")
	need(!strings.ContainsAny(c.root, " \n\r%\"\\"), "unsupported systemd installation path")
	need(exists(filepath.Join(c.root, "dreamtransctl")), "install Go tools before configuring background drain")
	unit := "dreamtrans-drain-" + c.state.Prefix
	need(!strings.ContainsAny(unit, "/ \n\r%\"\\"), "invalid deployment prefix")
	c.writeDrainUnits("/etc/systemd/system", unit)
	c.command("", "systemctl", "daemon-reload")
	c.command("", "systemctl", "enable", "--now", unit+".timer")
	save(filepath.Join(c.path, "drain-policy.json"), object{"enabled": true, "handoff_after_seconds": seconds, "unit": unit})
	c.progress("后台", fmt.Sprintf("自动排空已启用；切流 %d 秒后尝试客户端协作迁移（-1 表示只等待）；不会强制断开旧连接", seconds))
}

func (c *controller) writeDrainUnits(directory, unit string) {
	role := ""
	if c.edge() {
		role = " edge"
	}
	if c.yuaction() {
		role = " yuaction"
	}
	service := "[Unit]\nDescription=DreamTrans automatic safe drain\nAfter=docker.service network-online.target\n[Service]\nType=oneshot\nTimeoutStartSec=120\nExecStart=" + filepath.Join(c.root, "dreamtransctl") + role + " --dir " + c.root + " drain-tick\n"
	timer := "[Unit]\nDescription=Check DreamTrans release drain\n[Timer]\nOnBootSec=20\nOnUnitActiveSec=15\nAccuracySec=1\nUnit=" + unit + ".service\n[Install]\nWantedBy=timers.target\n"
	atomic(filepath.Join(directory, unit+".service"), []byte(service), 0o644)
	atomic(filepath.Join(directory, unit+".timer"), []byte(timer), 0o644)
}

func (c *controller) finishRelease(o *options) {
	if c.yuaction() {
		active, old := c.state.Active, c.state.Previous
		if old != "" && c.state.Phase == "draining" {
			c.probe(active)
			need(c.routeColor() == active, "active YuAction route not confirmed")
			if yes(obj(c.inspect("container", c.name(old))["State"])["Running"]) {
				c.control(old, "draining")
				c.control(old, "handoff")
			}
		}
	}

	if yes(c.drainPolicy()["enabled"]) {
		c.drainTick()
		if c.state.Phase == "draining" {
			c.progress("后台", "旧连接继续运行；后台会按配置尝试迁移并在排空后自动停止旧实例，可用 status 查看进度")
		}
		return
	}
	c.drain(o.drainTimeout)
}

func (c *controller) drainTick() {
	policy := c.drainPolicy()
	if !yes(policy["enabled"]) || c.state.Phase != "draining" {
		return
	}
	c.assertDatabase()
	// Retire immediately when all work and durable events have been acknowledged.
	c.drain(0)
	if c.state.Phase != "draining" {
		return
	}
	old, active := c.state.Previous, c.state.Active
	need(old != "" && old != active, "invalid drain ownership")
	seconds := number(policy["handoff_after_seconds"])
	if seconds < 0 {
		return
	}
	started, err := time.Parse(time.RFC3339Nano, c.state.DrainStartedAt)
	if err != nil {
		// Adoption of an old state starts a full grace period; never invent an
		// expired deadline for an already-running production session.
		c.state.DrainStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
		c.persist()
		return
	}
	if time.Since(started) < time.Duration(seconds)*time.Second {
		return
	}
	result := "waiting_for_client_or_task_completion"
	err = attempt(func() {
		// Never encourage users to leave a healthy old socket for a broken or
		// misrouted replacement. Retry on the next timer tick instead.
		c.probe(active)
		need(c.routeColor() == active, "active route not confirmed")
		s := c.control(old, "status")
		if !yes(s["handoff_supported"]) {
			result = "old_version_requires_natural_drain"
			return
		}
		c.control(old, "handoff")
	})
	if err != nil {
		result = "handoff_check_failed_retrying"
	}
	c.state.HandoffStatus = result
	c.persist()
	c.progress("迁移", result)
}
