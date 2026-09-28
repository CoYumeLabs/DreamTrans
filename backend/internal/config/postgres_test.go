package config

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// This database/sql driver deterministically gates reads and injects transaction
// failures without external database state or timing-dependent sleeps.
type configTestDatabase struct {
	mu                 sync.Mutex
	data               string
	missing            bool
	reads              int
	read               func(context.Context, string) error
	execErr, commitErr error
}
type configTestConnector struct{ db *configTestDatabase }
type configTestConn struct {
	db      *configTestDatabase
	inTx    bool
	pending string
}
type configTestRows struct {
	data string
	done bool
}

func (c configTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &configTestConn{db: c.db}, nil
}
func (c configTestConnector) Driver() driver.Driver { return c }
func (c configTestConnector) Open(string) (driver.Conn, error) {
	return c.Connect(context.Background())
}
func (c *configTestConn) Close() error { return nil }
func (c *configTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared statement")
}
func (c *configTestConn) Begin() (driver.Tx, error) { c.inTx = true; return c, nil }
func (c *configTestConn) Commit() error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	if c.db.commitErr != nil {
		c.inTx = false
		return c.db.commitErr
	}
	if c.pending != "" {
		c.db.data = c.pending
	}
	c.inTx, c.pending = false, ""
	return nil
}
func (c *configTestConn) Rollback() error { c.inTx, c.pending = false, ""; return nil }
func (c *configTestConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.db.mu.Lock()
	c.db.reads++
	data, missing, read := c.db.data, c.db.missing, c.db.read
	c.db.mu.Unlock()
	if read != nil {
		if err := read(ctx, query); err != nil {
			return nil, err
		}
	}
	return &configTestRows{data: data, done: missing}, nil
}
func (c *configTestConn) ExecContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	if c.db.execErr != nil {
		return nil, c.db.execErr
	}
	data, ok := args[0].Value.(string)
	if !ok {
		return nil, errors.New("unexpected config value")
	}
	if c.inTx {
		c.pending = data
	} else {
		c.db.data = data
	}
	return driver.RowsAffected(1), nil
}
func (r *configTestRows) Columns() []string { return []string{"value"} }
func (r *configTestRows) Close() error      { return nil }
func (r *configTestRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	values[0] = r.data
	return nil
}
func postgresConfigFixture(t *testing.T) *configTestDatabase {
	t.Helper()
	resetConfigForTest()
	t.Cleanup(resetConfigForTest)
	storage := &configTestDatabase{data: `{"models":{"chat_model":"initial-chat"}}`}
	db := sql.OpenDB(configTestConnector{db: storage})
	t.Cleanup(func() { _ = db.Close() })
	if err := LoadPostgres(db); err != nil {
		t.Fatal(err)
	}
	return storage
}
func expireConfigSnapshot() { mu.Lock(); sharedRefreshAfter = time.Time{}; mu.Unlock() }
func waitConfigSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("database operation did not reach its gate")
	}
}
func receiveConfig(t *testing.T, result <-chan Config) Config {
	t.Helper()
	select {
	case cfg := <-result:
		return cfg
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot reader blocked behind database I/O")
		return Config{}
	}
}
func configReadGate(t *testing.T, storage *configTestDatabase, transactional bool) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	storage.mu.Lock()
	storage.read = func(ctx context.Context, query string) error {
		if strings.Contains(query, "FOR UPDATE") != transactional {
			return nil
		}
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	storage.mu.Unlock()
	return entered, unblock
}
func TestPostgresConcurrentReadsUseSnapshotDuringOneRefresh(t *testing.T) {
	storage := postgresConfigFixture(t)
	entered, release := configReadGate(t, storage, false)
	expireConfigSnapshot()
	leader := make(chan Config, 1)
	go func() { leader <- Get() }()
	waitConfigSignal(t, entered)
	followers := make(chan Config, 5)
	for range 5 {
		go func() { followers <- Get() }()
	}
	for range 5 {
		if got := receiveConfig(t, followers).Models.Chat; got != "initial-chat" {
			t.Fatalf("concurrent reader lost successful snapshot: %s", got)
		}
	}
	storage.mu.Lock()
	reads := storage.reads
	storage.mu.Unlock()
	if reads != 2 {
		t.Fatalf("concurrent refreshes = %d, want 1", reads-1)
	}
	release()
	receiveConfig(t, leader)
}
func TestPostgresReadersContinueDuringBlockedUpdate(t *testing.T) {
	storage := postgresConfigFixture(t)
	entered, release := configReadGate(t, storage, true)
	expireConfigSnapshot()
	partial := &Config{}
	partial.Models.Chat = "committed-chat"
	updated := make(chan error, 1)
	go func() { updated <- Update(partial) }()
	waitConfigSignal(t, entered)
	read := make(chan Config, 1)
	go func() { read <- Get() }()
	if got := receiveConfig(t, read).Models.Chat; got != "initial-chat" {
		t.Fatalf("uncommitted update visible: %s", got)
	}
	release()
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if got := Get().Models.Chat; got != "committed-chat" {
		t.Fatalf("committed update not immediately visible: %s", got)
	}
}
func TestPostgresLateRefreshCannotOverwriteCommittedUpdate(t *testing.T) {
	storage := postgresConfigFixture(t)
	entered, release := configReadGate(t, storage, false)
	expireConfigSnapshot()
	leader := make(chan Config, 1)
	go func() { leader <- Get() }()
	waitConfigSignal(t, entered)
	partial := &Config{}
	partial.Models.Chat = "new-local-chat"
	if err := Update(partial); err != nil {
		t.Fatal(err)
	}
	release()
	if got := receiveConfig(t, leader).Models.Chat; got != "new-local-chat" {
		t.Fatalf("late read replaced committed configuration: %s", got)
	}
}
func TestPostgresRefreshObservesOtherReplicaAndUpdateMergesDatabaseState(t *testing.T) {
	storage := postgresConfigFixture(t)
	// Independent connection models another main instance committing a config.
	other := sql.OpenDB(configTestConnector{db: storage})
	defer func() { _ = other.Close() }()
	if _, err := other.Exec(`UPDATE deployment_metadata SET value=$1 WHERE key='server_config'`, `{"models":{"chat_model":"remote-chat"},"summary":{"max_lines":71}}`); err != nil {
		t.Fatal(err)
	}
	if got := Get().Models.Chat; got != "initial-chat" {
		t.Fatalf("fresh snapshot unexpectedly refreshed: %s", got)
	}
	expireConfigSnapshot()
	if got := Get().Models.Chat; got != "remote-chat" {
		t.Fatalf("remote committed update not refreshed: %s", got)
	}
	if _, err := other.Exec(`UPDATE deployment_metadata SET value=$1 WHERE key='server_config'`, `{"models":{"chat_model":"newer-remote-chat"},"summary":{"max_lines":83}}`); err != nil {
		t.Fatal(err)
	}
	partial := &Config{}
	partial.Summary.ParMinChars = 456
	if err := Update(partial); err != nil {
		t.Fatal(err)
	}
	cfg := Get()
	if cfg.Models.Chat != "newer-remote-chat" || cfg.Summary.MaxLines != 83 || cfg.Summary.ParMinChars != 456 {
		t.Fatalf("partial update did not merge latest database: %#v", cfg)
	}
	storage.mu.Lock()
	data := storage.data
	storage.mu.Unlock()
	var persisted Config
	if err := json.Unmarshal([]byte(data), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != cfg {
		t.Fatal("published snapshot differs from committed data")
	}
}
func TestPostgresFailedRefreshRetainsSnapshotAndBacksOff(t *testing.T) {
	storage := postgresConfigFixture(t)
	storage.mu.Lock()
	storage.read = func(context.Context, string) error { return context.DeadlineExceeded }
	storage.mu.Unlock()
	expireConfigSnapshot()
	for range 5 {
		if got := Get().Models.Chat; got != "initial-chat" {
			t.Fatalf("failed refresh lost successful snapshot: %s", got)
		}
	}
	storage.mu.Lock()
	reads := storage.reads
	storage.mu.Unlock()
	if reads != 2 {
		t.Fatalf("failed refresh retried without cooldown: %d reads", reads)
	}
}
func TestPostgresFailedWritesNeverPublishUncommittedState(t *testing.T) {
	for _, stage := range []string{"read", "decode", "exec", "commit"} {
		t.Run(stage, func(t *testing.T) {
			storage := postgresConfigFixture(t)
			storage.mu.Lock()
			switch stage {
			case "read":
				storage.read = func(context.Context, string) error { return errors.New("read rejected") }
			case "decode":
				storage.data = `{"models":{"chat_model":"must-not-leak"},"summary":{"max_lines":"invalid"}}`
			case "exec":
				storage.execErr = errors.New("write rejected")
			case "commit":
				storage.commitErr = errors.New("commit rejected")
			}
			storage.mu.Unlock()
			partial := &Config{}
			partial.Models.Chat = "must-not-publish"
			if err := Update(partial); err == nil {
				t.Fatal("injected database failure accepted")
			}
			if got := Get().Models.Chat; got != "initial-chat" {
				t.Fatalf("failed write leaked into snapshot: %s", got)
			}
		})
	}
}
func TestLoadPostgresFailsClosedWithoutImportedConfiguration(t *testing.T) {
	for _, data := range []string{"missing", "invalid-json"} {
		t.Run(data, func(t *testing.T) {
			resetConfigForTest()
			t.Cleanup(resetConfigForTest)
			storage := &configTestDatabase{data: data, missing: data == "missing"}
			db := sql.OpenDB(configTestConnector{db: storage})
			defer func() { _ = db.Close() }()
			if err := LoadPostgres(db); err == nil {
				t.Fatal("invalid startup configuration accepted")
			}
			mu.RLock()
			defer mu.RUnlock()
			if sharedDB != nil || current != (Config{}) {
				t.Fatal("failed startup published defaults or enabled failed backend")
			}
		})
	}
}

func TestPostgresLateRefreshCannotOverwriteReloadedBackend(t *testing.T) {
	storage := postgresConfigFixture(t)
	entered, release := configReadGate(t, storage, false)
	expireConfigSnapshot()
	leader := make(chan Config, 1)
	go func() { leader <- Get() }()
	waitConfigSignal(t, entered)
	replacement := &configTestDatabase{data: `{"models":{"chat_model":"replacement-chat"}}`}
	db := sql.OpenDB(configTestConnector{db: replacement})
	defer func() { _ = db.Close() }()
	if err := LoadPostgres(db); err != nil {
		t.Fatal(err)
	}
	release()
	if got := receiveConfig(t, leader).Models.Chat; got != "replacement-chat" {
		t.Fatalf("old backend read replaced current snapshot: %s", got)
	}
}
