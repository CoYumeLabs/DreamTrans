package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// SystemSettingsSnapshot is immutable and represents one database snapshot.
// Values stay private so callers cannot mutate another request's cached view.
type SystemSettingsSnapshot struct {
	Revision int64
	values   map[string]string
}

func (v *SystemSettingsSnapshot) Get(key string) (string, bool) {
	value, ok := v.values[key]
	return value, ok
}

// SettingsSnapshot validates the shared revision on every request. A write by
// any color (including an older binary) invalidates the next read. Failure to
// validate never serves stale policy. Financial transactions continue to read
// their settings through their own transaction, rather than this display cache.
func (s *Service) SettingsSnapshot(ctx context.Context) (*SystemSettingsSnapshot, error) {
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT revision FROM system_settings_revision WHERE singleton`).Scan(&revision); err != nil {
		return nil, err
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if s.settings != nil && s.settings.Revision == revision {
		return s.settings, nil
	}
	var raw []byte
	snapshot := &SystemSettingsSnapshot{}
	if err := s.db.QueryRowContext(ctx, `SELECT revision,COALESCE((SELECT jsonb_object_agg(key,value::text) FROM system_settings),'{}'::jsonb) FROM system_settings_revision WHERE singleton`).Scan(&snapshot.Revision, &raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &snapshot.values); err != nil {
		return nil, err
	}
	s.settings = snapshot
	return snapshot, nil
}

func (s *Service) invalidateSettings() { s.settingsMu.Lock(); s.settings = nil; s.settingsMu.Unlock() }

func (s *Service) cachedSystemSetting(ctx context.Context, key string) (string, error) {
	snapshot, err := s.SettingsSnapshot(ctx)
	if err != nil {
		return "", err
	}
	value, ok := snapshot.Get(key)
	if !ok {
		return "", sql.ErrNoRows
	}
	return value, nil
}

type TrainingSettings struct {
	Enabled         bool
	DiscountPercent float64
}

// TrainingSettings reads availability and discount from one consistent view.
func (s *Service) TrainingSettings(ctx context.Context) TrainingSettings {
	if !s.trainingProgram {
		return TrainingSettings{}
	}
	snapshot, err := s.SettingsSnapshot(ctx)
	if err != nil {
		return TrainingSettings{}
	}
	enabled := true
	if raw, ok := snapshot.Get(trainingProgramEnabledKey); ok {
		enabled = parseJSONBool(raw, true)
	}
	if !enabled {
		return TrainingSettings{}
	}
	discount := DefaultTrainingDiscountPercent
	if raw, ok := snapshot.Get(trainingDiscountSettingKey); ok {
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100 {
			discount = value
		}
	}
	return TrainingSettings{Enabled: true, DiscountPercent: discount}
}
