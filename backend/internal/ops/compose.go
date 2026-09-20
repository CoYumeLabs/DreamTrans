package ops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func composeCommand(ctx context.Context, o *options, out io.Writer) {
	switch o.action {
	case "compose-files":
		composeFiles(o.root, out)
	case "compose-normalize":
		need(len(o.extra) >= 2, "compose-normalize POSTGRES_IMAGE FILE...")
		normalizeCompose(o.extra[0], o.extra[1:])
	case "compose-validate":
		need(len(o.extra) == 4, "compose-validate BEFORE AFTER IMAGE_TAG POSTGRES_IMAGE")
		validateCompose(o.extra)
	case "compose-volume":
		need(len(o.extra) == 4, "compose-volume SERVICE TARGET CONTAINER REQUIRE_MANAGED")
		data, e := io.ReadAll(os.Stdin)
		check(e, "cannot read merged Compose configuration")
		c := &controller{ctx: ctx, run: execute}
		name := c.confirmVolume(obj(decode(data)), o.extra)
		_, _ = fmt.Fprintln(out, name)
	default:
		fail("unknown Compose validation helper")
	}
}
func composeFiles(root string, out io.Writer) {
	root, e := filepath.Abs(root)
	check(e, "invalid installation directory")
	root, e = filepath.EvalSymlinks(root)
	check(e, "installation directory unavailable")
	b, e := io.ReadAll(os.Stdin)
	check(e, "cannot read Compose environment")
	env := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			env[k] = v
		}
	}
	var files []string
	if env["COMPOSE_FILE"] != "" {
		sep := env["COMPOSE_PATH_SEPARATOR"]
		if sep == "" {
			sep = string(os.PathListSeparator)
		}
		files = strings.Split(env["COMPOSE_FILE"], sep)
	} else {
		need(!exists(filepath.Join(root, "compose.yaml")) && !exists(filepath.Join(root, "compose.yml")), "set COMPOSE_FILE explicitly for nonstandard default Compose files")
		files = []string{"docker-compose.yml"}
		if exists(filepath.Join(root, "docker-compose.override.yml")) {
			files = append(files, "docker-compose.override.yml")
		}
	}
	seen := map[string]bool{}
	for _, file := range append([]string{"docker-compose.yml"}, files...) {
		if !filepath.IsAbs(file) {
			file = filepath.Join(root, file)
		}
		if seen[file] {
			continue
		}
		seen[file] = true
		info, e := os.Lstat(file)
		check(e, "Compose file unavailable")
		stat, ok := info.Sys().(*syscall.Stat_t)
		need(ok && info.Mode().IsRegular() && int(stat.Uid) == os.Geteuid() && filepath.Dir(file) == root && !strings.ContainsAny(file, "\r\n"), "unsafe or unsupported Compose file")
		_, _ = fmt.Fprintln(out, file)
	}
}

var servicesLine = regexp.MustCompile(`^["']?services["']?:\s*(?:#.*)?$`)
var serviceLine = regexp.MustCompile(`^["']?([\w.-]+)["']?:\s*(?:#.*)?$`)
var scalarLine = regexp.MustCompile(`^(\s*)(image|pull_policy):\s*(["']?)([^\s"'#]+)(["']?)\s*(#.*)?$`)

func normalizeCompose(postgres string, paths []string) {
	recovery := false
	contents := map[string]string{}
	for _, path := range paths {
		//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
		b, e := os.ReadFile(path)
		check(e, "cannot read Compose configuration")
		contents[path] = string(b)
		recovery = recovery || strings.Contains(string(b), "dreamtrans-migration/")
	}
	if !recovery {
		return
	}
	for _, path := range paths {
		lines := strings.SplitAfter(contents[path], "\n")
		servicesIndent, serviceIndent := -1, -1
		service := ""
		for i, line := range lines {
			clean := strings.TrimSpace(line)
			if clean == "" || strings.HasPrefix(clean, "#") {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " \t\r\n"))
			if servicesLine.MatchString(clean) {
				servicesIndent = indent
				serviceIndent = -1
				continue
			}
			if servicesIndent < 0 {
				continue
			}
			if indent <= servicesIndent {
				servicesIndent = -1
				service = ""
				continue
			}
			key := serviceLine.FindStringSubmatch(clean)
			if serviceIndent < 0 && key != nil {
				serviceIndent = indent
			}
			if indent == serviceIndent {
				service = ""
				if key != nil {
					service = key[1]
				}
				continue
			}
			if service != "app" && service != "db" && service != "migrate" {
				continue
			}
			match := scalarLine.FindStringSubmatch(strings.TrimSuffix(line, "\n"))
			if len(match) != 7 || match[3] != match[5] {
				continue
			}
			value := match[4]
			if match[2] == "image" && strings.HasPrefix(value, "dreamtrans-migration/") {
				value = postgres
				if service == "app" {
					value = "ghcr.io/coyumelabs/dreamtrans:${IMAGE_TAG:-latest}"
				}
			} else if match[2] == "pull_policy" && value == "never" {
				value = "missing"
			} else {
				continue
			}
			lines[i] = match[1] + match[2] + ": " + match[3] + value + match[5]
			if match[6] != "" {
				lines[i] += " " + match[6]
			}
			lines[i] += "\n"
		}
		updated := strings.Join(lines, "")
		if updated != contents[path] {
			info, e := os.Stat(path)
			check(e, "cannot inspect Compose file")
			atomic(path, []byte(updated), info.Mode().Perm())
		}
	}
}
func validateCompose(a []string) {
	before, after := load(a[0]), load(a[1])
	for _, service := range []string{"app", "db", "migrate"} {
		old := obj(obj(before["services"])[service])
		next := obj(obj(after["services"])[service])
		if strings.HasPrefix(str(old["image"]), "dreamtrans-migration/") {
			expected := a[3]
			if service == "app" {
				expected = "ghcr.io/coyumelabs/dreamtrans:" + a[2]
			}
			need(str(next["image"]) == expected && str(next["pull_policy"]) != "never", "recovery image conversion could not be verified")
		}
		delete(old, "image")
		delete(old, "pull_policy")
		delete(next, "image")
		delete(next, "pull_policy")
	}
	need(bytes.Equal(marshal(before), marshal(after)), "recovery conversion changed settings beyond application images/pull policies")
}
func (c *controller) confirmVolume(config object, a []string) string {
	service, target, container, managed := a[0], a[1], a[2], a[3]
	current := c.inspect("container", container)
	labels := obj(obj(current["Config"])["Labels"])
	need(str(labels["com.docker.compose.project"]) == str(config["name"]) && str(labels["com.docker.compose.service"]) == service, "container does not belong to selected Compose service")
	planned, actual := []object{}, []object{}
	for _, v := range list(obj(obj(config["services"])[service])["volumes"]) {
		m := obj(v)
		if str(m["target"]) == target {
			planned = append(planned, m)
		}
	}
	for _, v := range list(current["Mounts"]) {
		m := obj(v)
		if str(m["Destination"]) == target {
			actual = append(actual, m)
		}
	}
	need(len(planned) == 1 && len(actual) == 1, "expected exactly one production data mount")
	p, m := planned[0], actual[0]
	need(str(p["type"]) == "volume" && str(m["Type"]) == "volume" && !yes(p["read_only"]) && yes(m["RW"]), "data mount must be a writable named volume")
	need(str(obj(p["volume"])["subpath"]) == "", "volume subpaths cannot be migrated recursively")
	key := str(p["source"])
	definition := obj(obj(config["volumes"])[key])
	name := str(definition["name"])
	need(!yes(definition["external"]) || managed != "true", "external volumes require an existing production container")
	need(name != "" && name == str(m["Name"]), "merged Compose volume differs from actual container")
	v := c.inspect("volume", name)
	need(str(v["Name"]) == name && str(v["Driver"]) == "local" && len(obj(v["Options"])) == 0, "expected an existing local volume without mount options")
	if !yes(definition["external"]) {
		owner := obj(v["Labels"])
		need(str(owner["com.docker.compose.project"]) == str(config["name"]) && str(owner["com.docker.compose.volume"]) == key, "managed volume ownership mismatch")
	}
	return name
}
func pruneBackups(args []string) {
	need(len(args) == 2, "backup-prune DIRECTORY KEEP")
	keep, e := strconv.Atoi(args[1])
	check(e, "invalid backup retention count")
	need(keep >= 1, "BACKUP_LOCAL_KEEP must be positive")
	paths, e := filepath.Glob(filepath.Join(args[0], "dreamtrans-*.full.tar.enc"))
	check(e, "invalid backup directory")
	type entry struct {
		path string
		info os.FileInfo
	}
	files := make([]entry, 0, len(paths))
	for _, p := range paths {
		info, e := os.Lstat(p)
		check(e, "cannot inspect retained backup")
		need(info.Mode().IsRegular(), "backup retention refuses non-regular files")
		files = append(files, entry{p, info})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().After(files[j].info.ModTime()) })
	for i := keep; i < len(files); i++ {
		check(os.Remove(files[i].path), "cannot prune local backup")
	}
}
