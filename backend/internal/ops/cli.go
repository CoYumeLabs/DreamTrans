package ops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type options struct {
	root, action, image, proxyImage, app, database, databaseNetwork, output, mainURL, tunnelImage, registrationFile, providerKeyFile, tunnelTokenFile, origins, backupFile string
	observe, drainTimeout, handoffAfter, port, maximum                                                                                                                     int
	edge, maintenance, pause, training                                                                                                                                     bool
	extra                                                                                                                                                                  []string
}

func parse(args []string, errOut io.Writer) *options {
	o := options{root: "/root/dreamtrans", observe: 60, drainTimeout: 60, port: 16002, maximum: 8}
	if len(args) > 0 && args[0] == "edge" {
		o.edge = true
		o.root = "/opt/dreamtrans-edge"
		o.port = 16003
		args = args[1:]
	}
	flags := flag.NewFlagSet("dreamtransctl", flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.StringVar(&o.root, "dir", o.root, "installation directory")
	flags.StringVar(&o.image, "image", "", "immutable release image (upgrade defaults to verified main latest)")
	flags.StringVar(&o.proxyImage, "proxy-image", "", "immutable proxy image")
	flags.StringVar(&o.app, "app", "", "legacy application container")
	flags.StringVar(&o.database, "database", "", "existing database container")
	flags.StringVar(&o.databaseNetwork, "database-network", "", "existing shared network")
	flags.StringVar(&o.output, "output", "", "new snapshot destination")
	flags.StringVar(&o.mainURL, "main", "", "main HTTPS origin")
	flags.StringVar(&o.tunnelImage, "tunnel-image", "", "immutable tunnel image")
	flags.StringVar(&o.registrationFile, "registration-file", "", "private registration file")
	flags.StringVar(&o.providerKeyFile, "provider-key-file", "", "private provider key file")
	flags.StringVar(&o.tunnelTokenFile, "tunnel-token-file", "", "private tunnel token file")
	flags.StringVar(&o.origins, "origins", "", "comma-separated browser origins")
	flags.StringVar(&o.backupFile, "backup-file", "", "verified backup helper to install alongside CLI")
	flags.IntVar(&o.observe, "observe", 60, "observation seconds")
	flags.IntVar(&o.drainTimeout, "drain-timeout", 60, "wait for drain; never forcibly end active sessions")
	flags.IntVar(&o.handoffAfter, "handoff-after", 600, "background grace seconds before cooperative handoff; -1 only waits")
	flags.IntVar(&o.port, "port", o.port, "local entrance port")
	flags.IntVar(&o.maximum, "maximum", 8, "Edge session capacity")
	flags.BoolVar(&o.maintenance, "maintenance", false, "initial conversion maintenance window")
	flags.BoolVar(&o.pause, "pause", false, "pause before switching")
	flags.BoolVar(&o.training, "training", false, "use a dedicated training provider credential")
	var normalized []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			normalized = append(normalized, arg)
			key := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
			f := flags.Lookup(key)
			if f != nil && !strings.Contains(arg, "=") {
				if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool {
					need(i+1 < len(args), "missing flag value")
					i++
					normalized = append(normalized, args[i])
				}
			}
		} else if o.action == "" {
			o.action = arg
		} else {
			o.extra = append(o.extra, arg)
		}
	}
	if err := flags.Parse(normalized); errors.Is(err, flag.ErrHelp) {
		o.action = "help"
	} else {
		check(err, "invalid command flags")
	}
	need(o.observe >= 0 && o.drainTimeout >= 0, "timeouts must be non-negative")
	if o.action == "" {
		o.action = "help"
		if o.edge {
			o.action = "install"
		}
	}
	return &o
}

// Run executes a lifecycle command and returns sanitized operational errors.
func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	return attempt(func() {
		o := parse(args, errOut)
		if o.action == "help" {
			_, _ = fmt.Fprintln(out, "DreamTrans Go 运维工具\n  dreamtransctl --dir /root/dreamtrans upgrade\n  dreamtransctl --dir DIR deploy --image REPOSITORY@sha256:DIGEST\n  dreamtransctl --dir DIR status|resume|drain|rollback|abort|sync-entry|diagnose\n  dreamtransctl --dir DIR configure-drain --handoff-after 600\n  dreamtransctl --dir DIR snapshot --output FILE\n  dreamtransctl --dir DIR install-tools --backup-file FILE\n  dreamtransctl edge --dir DIR install|upgrade|status|drain|rollback|resume|abort|logs|diagnose|uninstall|converge|pause-releases|resume-releases|reconcile\n  首次主站转换：init --app NAME --database NAME --image DIGEST --proxy-image DIGEST --maintenance")
			return
		}
		if strings.HasPrefix(o.action, "compose-") {
			composeCommand(ctx, o, out)
			return
		}
		if o.action == "backup-prune" {
			pruneBackups(o.extra)
			return
		}
		c := newController(ctx, o.root, out, errOut)
		unlock := lock(filepath.Join(c.path, "lock"))
		defer unlock()
		if o.action == "install-tools" {
			need(c.state == nil || c.edge() == o.edge, "installation role mismatch")
			if o.edge {
				need(c.state != nil, "Edge adoption requires an existing registered installation")
				o.backupFile = ""
			}
			c.installTools(o.backupFile)
			return
		}
		if o.action == "status" {
			_, _ = out.Write(marshal(c.status()))
			return
		}
		if o.action == "init" {
			need(!o.edge, "use edge install")
			c.initMain(o)
			return
		}
		if o.action == "install" && o.edge {
			c.installEdge(o)
			return
		}
		need(c.state != nil, "no existing installation; run initial conversion or Edge install")
		need(c.edge() == o.edge, "installation role mismatch; use the appropriate main or edge command")
		c.dispatch(o)
	})
}
func (c *controller) dispatch(o *options) {
	switch o.action {
	case "deploy", "upgrade":
		if c.edge() && o.image == "" {
			c.converge(o)
		} else {
			c.deploy(o)
		}
	case "resume":
		c.resume(o)
	case "configure-drain":
		c.configureDrain(o.handoffAfter)
	case "drain-tick":
		c.drainTick()
	case "drain":
		if c.edge() {
			c.drainNode()
		} else {
			c.drain(o.drainTimeout)
		}
	case "rollback":
		c.rollback()
	case "abort":
		c.abort()
	case "sync-entry":
		need(!c.edge(), "main-site only")
		c.syncEntry()
		c.progress("入口", "YuAction 原网络固定入口已核对")
	case "snapshot":
		c.snapshot(o.output)
	case "diagnose":
		if c.edge() {
			_, _ = c.out.Write(marshal(c.status()))
		} else {
			c.diagnose()
		}
	case "logs":
		_, _ = fmt.Fprintln(c.out, c.docker("logs", "--tail", "100", c.name(str(c.state["active"]))))
	case "state-field":
		need(len(o.extra) == 1 && (o.extra[0] == "database_id" || o.extra[0] == "active_image"), "state-field only exposes database_id or active_image")
		v := c.state[o.extra[0]]
		if o.extra[0] == "active_image" {
			v = obj(obj(c.state["colors"])[str(c.state["active"])])["image"]
		}
		_, _ = fmt.Fprintln(c.out, str(v))
	case "pause-releases", "resume-releases":
		need(c.edge(), "Edge-only operation")
		c.state["release_paused"] = o.action == "pause-releases"
		c.persist()
	case "converge":
		need(c.edge(), "Edge-only operation")
		c.converge(o)
	case "reconcile":
		need(c.edge(), "Edge-only operation")
		c.reconcile()
	case "uninstall":
		need(c.edge(), "Edge-only operation")
		c.uninstall()
	default:
		fail("unknown operation; run dreamtransctl help")
	}
}
func (c *controller) latest() string {
	const repository = "ghcr.io/coyumelabs/dreamtrans"
	c.progress("版本", "查询 main 已通过 CI 发布的最新版")
	c.docker("pull", repository+":latest")
	info := c.inspect("image", repository+":latest")
	labels := obj(obj(info["Config"])["Labels"])
	revision := str(labels["org.opencontainers.image.revision"])
	need(regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(revision), "latest image lacks an immutable source revision")
	need(str(labels["org.opencontainers.image.source"]) == "https://github.com/CoYumeLabs/DreamTrans", "latest image has unexpected source metadata")
	for _, v := range list(info["RepoDigests"]) {
		ref := str(v)
		if strings.HasPrefix(ref, repository+"@") && immutable.MatchString(ref) {
			c.progress("版本", "已固定版本 "+revision[:12]+" / "+ref)
			return ref
		}
	}
	fail("latest image lacks a verified repository digest")
	return ""
}
func (c *controller) installTools(backup string) {
	if c.state != nil {
		c.assertDatabase()
	}
	if c.edge() {
		need(!strings.ContainsAny(c.root, " \n\r%\"\\"), "unsupported systemd installation path")
	}
	executable, e := os.Executable()
	check(e, "cannot locate Go CLI binary")
	destination := filepath.Join(c.root, "dreamtransctl")
	if executable != destination {
		if exists(destination) {
			copyFile(destination, destination+".previous", 0o700)
		}
		copyFile(executable, destination, 0o700)
	}
	if backup != "" {
		if exists(filepath.Join(c.root, "backup.sh")) {
			copyFile(filepath.Join(c.root, "backup.sh"), filepath.Join(c.root, "backup.sh.previous"), 0o700)
		}
		copyFile(backup, filepath.Join(c.root, "backup.sh"), 0o700)
	}
	if c.state != nil && c.edge() {
		need(!strings.ContainsAny(c.root, " \n\r%\"\\"), "unsupported systemd installation path")
		unit := "dreamtrans-edge-" + str(c.state["prefix"])
		service := "[Unit]\nDescription=DreamTrans Edge release reconciliation\nAfter=docker.service network-online.target\n[Service]\nType=oneshot\nExecStart=" + destination + " edge --dir " + c.root + " converge\n"
		timer := "[Unit]\nDescription=Poll authorized Edge release requests\n[Timer]\nOnBootSec=60\nOnUnitActiveSec=60\nUnit=" + unit + ".service\n[Install]\nWantedBy=timers.target\n"
		atomic("/etc/systemd/system/"+unit+".service", []byte(service), 0o644)
		atomic("/etc/systemd/system/"+unit+".timer", []byte(timer), 0o644)
		c.command("", "systemctl", "daemon-reload")
		c.command("", "systemctl", "enable", "--now", unit+".timer")
	}
	c.progress("✓", "Go 运维工具已安装；保留原配置、数据卷与备份计划")
}
