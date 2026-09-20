package ops

import (
	"archive/tar"
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func addTar(w *tar.Writer, path, name string, ancestors map[string]bool) {
	info, e := os.Stat(path)
	check(e, "backup source missing or unreadable")
	need(info.Mode().IsRegular() || info.IsDir(), "unsupported backup file type")
	resolved, e := filepath.EvalSymlinks(path)
	check(e, "cannot resolve backup source")
	need(!ancestors[resolved], "cyclic configuration symlink")
	header, e := tar.FileInfoHeader(info, "")
	check(e, "cannot construct backup header")
	header.Name = filepath.ToSlash(name)
	check(w.WriteHeader(header), "cannot write backup header")
	if info.IsDir() {
		ancestors[resolved] = true
		defer delete(ancestors, resolved)
		entries, e := os.ReadDir(path)
		check(e, "cannot list backup directory")
		for _, entry := range entries {
			child := filepath.Join(name, entry.Name())
			if filepath.ToSlash(child) == ".bluegreen/migration" || filepath.ToSlash(child) == ".bluegreen/lock" {
				continue
			}
			addTar(w, filepath.Join(path, entry.Name()), child, ancestors)
		}
		return
	}
	//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
	f, e := os.Open(path)
	check(e, "cannot read backup file")
	defer func() { _ = f.Close() }()
	_, e = io.Copy(w, f)
	check(e, "cannot copy backup content")
}
func tarFiles(destination string, files map[string]string) {
	//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
	f, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	check(e, "backup destination already exists or cannot be created")
	defer func() { _ = f.Close() }()
	w := tar.NewWriter(f)
	defer func() { _ = w.Close() }()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		addTar(w, files[name], name, map[string]bool{})
	}
	check(w.Close(), "cannot finalize backup archive")
	check(f.Sync(), "cannot sync backup archive")
}
func (c *controller) archiveConfiguration(destination string) {
	files := map[string]string{}
	for _, directory := range []string{c.root, filepath.Join(c.root, "yuaction")} {
		for _, pattern := range []string{".env", ".env.*", "docker-compose.yml", "docker-compose.yaml", "docker-compose.*.yml", "docker-compose.*.yaml", "compose.yml", "compose.yaml", "compose.*.yml", "compose.*.yaml"} {
			paths, e := filepath.Glob(filepath.Join(directory, pattern))
			check(e, "invalid configuration pattern")
			for _, path := range paths {
				info, e := os.Stat(path)
				if e == nil && info.Mode().IsRegular() {
					name, e := filepath.Rel(c.root, path)
					check(e, "invalid configuration location")
					files[name] = path
				}
			}
		}
	}
	for _, name := range []string{"backup.sh", "dreamtransctl", "release.py", "yuaction/install.sh", "yuaction/.project", "yuaction/.dreamtrans-dir"} {
		path := filepath.Join(c.root, name)
		if exists(path) {
			files[name] = path
		}
	}
	files[".bluegreen"] = c.path
	tarFiles(destination, files)
}
func (c *controller) backupLock() (func(), func()) {
	ctx, cancel := context.WithCancel(c.ctx)
	a := append([]string{"exec", "-i"}, envArgs(obj(c.state["database_env"]))...)
	a = append(a, str(c.state["database_id"]), "psql", "-XAt", "-v", "ON_ERROR_STOP=1")
	cmd := exec.CommandContext(ctx, "docker", a...) //nolint:gosec // Fixed executable and explicit Docker/psql arguments, credentials never logged.
	in, e := cmd.StdinPipe()
	check(e, "cannot open backup lock input")
	out, e := cmd.StdoutPipe()
	check(e, "cannot open backup lock output")
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		cancel()
		check(e, "cannot start backup lock session")
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	release := func() {
		_ = in.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancel()
			<-done
		}
		cancel()
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()
	_, e = io.WriteString(in, "SELECT 'locked' FROM pg_advisory_lock(1146243412,54);\n")
	check(e, "backup lock session failed")
	acquired := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "locked" {
				acquired <- true
				return
			}
		}
		acquired <- false
	}()
	select {
	case success := <-acquired:
		need(success, "backup lock connection failed")
	case <-time.After(30 * time.Second):
		fail("backup could not acquire file-retention lock")
	case <-c.ctx.Done():
		fail("backup interrupted")
	}
	ok = true
	// Verify the lock holder is alive immediately before and after snapshot work.
	return release, func() {
		select {
		case <-done:
			fail("backup lock connection lost; snapshot not publishable")
		default:
		}
	}
}
func (c *controller) snapshot(output string) {
	need(!c.edge(), "main-site snapshots cannot be run on an Edge")
	c.assertDatabase()
	active := str(c.state["active"])
	_, ok := obj(c.state["colors"])[active]
	need(active != "" && ok, "initial conversion incomplete; preserve pre-conversion backup")
	destination, e := filepath.Abs(output)
	check(e, "invalid backup path")
	need(output != "" && !exists(destination), "backup output must be a new file")
	mkdir(filepath.Dir(destination))
	stage, e := os.MkdirTemp(filepath.Dir(destination), ".snapshot-")
	check(e, "cannot create snapshot staging directory")
	defer func() { _ = os.RemoveAll(stage) }()
	unlock, assertLock := c.backupLock()
	defer unlock()
	assertLock()
	envfile := filepath.Join(stage, "database.env")
	atomic(envfile, envBytes(obj(c.state["database_env"])), 0o600)
	c.docker("run", "--rm", "--network", str(c.state["database_network"]), "--env-file", envfile, "--mount", "type=volume,src="+str(c.state["application_volume"])+",dst=/application,readonly", "--mount", "type=bind,src="+stage+",dst=/snapshot", "--entrypoint", "/bin/sh", str(c.state["database_image"]), "-ec", "pg_dump -Fc > /snapshot/database.dump; tar -C /application -cf /snapshot/application.tar .; pg_restore --list /snapshot/database.dump >/dev/null; tar -tf /snapshot/application.tar >/dev/null")
	c.archiveConfiguration(filepath.Join(stage, "configuration.tar"))
	hashes := object{}
	files := map[string]string{}
	for _, name := range []string{"database.dump", "application.tar", "configuration.tar"} {
		path := filepath.Join(stage, name)
		hashes[name] = hashFile(path)
		files[name] = path
	}
	save(filepath.Join(stage, "manifest.json"), object{"format": 1, "database_volume": c.state["database_volume"], "application_volume": c.state["application_volume"], "active_image": obj(obj(c.state["colors"])[active])["image"], "files": hashes})
	files["manifest.json"] = filepath.Join(stage, "manifest.json")
	snapshot := filepath.Join(stage, "snapshot.tar")
	tarFiles(snapshot, files)
	assertLock()
	check(os.Link(snapshot, destination), "cannot publish backup (output may already exist)")
	directory, e := os.Open(filepath.Dir(destination))
	check(e, "cannot open backup directory")
	defer func() { _ = directory.Close() }()
	check(directory.Sync(), "cannot sync published backup directory")
	c.progress("备份", "数据库、应用卷与部署配置快照已创建并附带校验清单")
}
