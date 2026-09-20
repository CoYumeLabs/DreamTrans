package ops

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const databaseReport = `
BEGIN READ ONLY;
SET LOCAL statement_timeout = '5s';
SET LOCAL lock_timeout = '1s';
SELECT name,setting,unit FROM pg_settings WHERE name IN
 ('shared_buffers','work_mem','effective_cache_size','max_connections',
  'track_io_timing','max_parallel_workers_per_gather','jit') ORDER BY name;
SELECT datname,numbackends,blks_read,blks_hit,temp_files,
 pg_size_pretty(temp_bytes) AS temp_written,deadlocks,stats_reset
 FROM pg_stat_database WHERE datname=current_database();
SELECT state,wait_event_type,wait_event,count(*) AS connections,
 max(clock_timestamp()-query_start) FILTER (WHERE state='active') AS active_age
 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()
 GROUP BY state,wait_event_type,wait_event ORDER BY connections DESC;
SELECT schemaname,relname,n_live_tup,n_dead_tup,n_mod_since_analyze,
 seq_scan,idx_scan,last_analyze,last_autoanalyze,
 pg_size_pretty(pg_total_relation_size(relid)) AS total_size
 FROM pg_stat_user_tables ORDER BY pg_total_relation_size(relid) DESC LIMIT 20;
SELECT tablename,indexname,indexdef FROM pg_indexes
 WHERE schemaname='public' AND tablename IN
 ('transcripts','sessions','usage_logs','balance_transactions')
 ORDER BY tablename,indexname;
SELECT EXISTS(SELECT 1 FROM pg_extension
 WHERE extname='pg_stat_statements') AS statement_statistics_installed;
COMMIT;
`

func (c *controller) diagnose() {
	need(!c.edge() && str(c.state["active"]) != "", "requires a converted main site")
	c.assertDatabase()
	_, _ = fmt.Fprintln(c.out, "[1/4] Host resources (current sample; not EC2 credit history)", time.Now().UTC().Format(time.RFC3339))
	for _, file := range []string{"/proc/loadavg", "/proc/meminfo"} {
		//nolint:gosec // Operator-selected host files; CLI is not exposed through the application API.
		b, e := os.ReadFile(file)
		check(e, "cannot inspect host resources")
		for _, line := range strings.Split(string(b), "\n") {
			if file == "/proc/loadavg" || strings.HasPrefix(line, "MemTotal:") || strings.HasPrefix(line, "MemAvailable:") || strings.HasPrefix(line, "SwapTotal:") || strings.HasPrefix(line, "SwapFree:") {
				_, _ = fmt.Fprintln(c.out, line)
			}
		}
	}
	var fs unix.Statfs_t
	check(unix.Statfs(c.root, &fs), "cannot inspect filesystem capacity")
	_, _ = fmt.Fprintf(c.out, "Install filesystem free GiB: %.2f\n", float64(fs.Bavail)*float64(fs.Bsize)/(1<<30))
	if _, e := exec.LookPath("vmstat"); e == nil {
		_, _ = fmt.Fprintln(c.out, c.command("", "vmstat", "1", "3"))
	}
	_, _ = fmt.Fprintln(c.out, "[2/4] Active application, proxy and database resources")
	_, _ = fmt.Fprintln(c.out, c.docker("stats", "--no-stream", "--format", "{{.Name}} CPU={{.CPUPerc}} MEMORY={{.MemUsage}} BLOCK_IO={{.BlockIO}} PIDS={{.PIDs}}", str(c.state["database_id"]), c.name(str(c.state["active"])), c.name("proxy")))
	_, _ = fmt.Fprintln(c.out, "[3/4] Local entry latency (does not measure browser/Cloudflare)")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	host := str(c.state["bind"])
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		for sample := 1; sample <= 2; sample++ {
			start := time.Now()
			status := "failed/timeout"
			req, e := http.NewRequestWithContext(c.ctx, http.MethodGet, fmt.Sprintf("http://%s:%d%s", host, number(c.state["port"]), path), http.NoBody)
			check(e, "invalid local entrance")
			response, e := client.Do(req)
			if e == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				_ = response.Body.Close()
				status = fmt.Sprint(response.StatusCode)
			}
			_, _ = fmt.Fprintf(c.out, "%s sample=%d status=%s ms=%.1f\n", path, sample, status, float64(time.Since(start).Microseconds())/1000)
		}
	}
	_, _ = fmt.Fprintln(c.out, "[4/4] Read-only PostgreSQL statistics and existing indexes")
	_, _ = fmt.Fprintln(c.out, c.pg(databaseReport))
	_, _ = fmt.Fprintln(c.out, "No index changes, ANALYZE, restarts, query text, or account content included.")
}
