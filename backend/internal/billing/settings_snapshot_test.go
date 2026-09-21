package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestSettingsSnapshotInvalidatesAcrossProcessesAndLegacyWrites(t *testing.T) {
	db := integrationDB(t)
	writer := newIntegrationService(t, db)
	reader := NewService(db)
	writer.SetTrainingProgramAvailable(true)
	reader.SetTrainingProgramAvailable(true)
	const key = "audit_settings_snapshot_fixture"
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM system_settings WHERE key=$1`, key) })
	if err := writer.SetSystemSettings(t.Context(), map[string]string{key: `{"nested":true}`, trainingProgramEnabledKey: "true", trainingDiscountSettingKey: "23"}, nil); err != nil {
		t.Fatal(err)
	}
	first, err := reader.SettingsSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := reader.SettingsSnapshot(t.Context())
	if err != nil || first != second {
		t.Fatal("unchanged revision failed to reuse immutable snapshot")
	}
	if value, ok := first.Get(key); !ok || value != `{"nested": true}` {
		t.Fatalf("JSONB text changed: %q", value)
	}
	if setting := reader.TrainingSettings(t.Context()); !setting.Enabled || setting.DiscountPercent != 23 {
		t.Fatalf("inconsistent training snapshot: %+v", setting)
	}
	if err := writer.SetSystemSettings(t.Context(), map[string]string{trainingProgramEnabledKey: "false", trainingDiscountSettingKey: "31"}, nil); err != nil {
		t.Fatal(err)
	}
	if setting := reader.TrainingSettings(t.Context()); setting.Enabled || setting.DiscountPercent != 0 {
		t.Fatal("other process served stale policy")
	}
	// Simulate a previous-version color or operator SQL which bypasses Service.
	if _, err := db.Exec(`UPDATE system_settings SET value='true'::jsonb WHERE key=$1`, trainingProgramEnabledKey); err != nil {
		t.Fatal(err)
	}
	if setting := reader.TrainingSettings(t.Context()); !setting.Enabled || setting.DiscountPercent != 31 {
		t.Fatal("legacy SQL did not invalidate the cached settings")
	}
	if value, _ := first.Get(trainingDiscountSettingKey); value != "23" {
		t.Fatal("an old immutable snapshot changed beneath its caller")
	}
	if _, err := db.Exec(`DELETE FROM system_settings WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetSystemSetting(t.Context(), key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted setting remained cached: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.SettingsSnapshot(ctx); err == nil {
		t.Fatal("failed revision read returned stale cached settings")
	}
}
