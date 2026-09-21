package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// deploymentState is the versioned host-local lifecycle journal. Unknown fields
// are retained verbatim so an older controller never erases a newer extension.
type deploymentState struct {
	Prefix                string                     `json:"prefix,omitempty"`
	Role                  string                     `json:"role,omitempty"`
	Phase                 string                     `json:"phase,omitempty"`
	Target                string                     `json:"target,omitempty"`
	Network               string                     `json:"network,omitempty"`
	Active                string                     `json:"active,omitempty"`
	Previous              string                     `json:"previous,omitempty"`
	InitialImage          string                     `json:"initial_image,omitempty"`
	DatabaseNetwork       string                     `json:"database_network,omitempty"`
	DatabaseID            string                     `json:"database_id,omitempty"`
	DatabaseImage         string                     `json:"database_image,omitempty"`
	DatabaseVolume        string                     `json:"database_volume,omitempty"`
	ApplicationVolume     string                     `json:"application_volume,omitempty"`
	LegacyID              string                     `json:"legacy_id,omitempty"`
	LegacyRestart         string                     `json:"legacy_restart,omitempty"`
	LegacyFrontend        string                     `json:"legacy_frontend,omitempty"`
	ProxyImage            string                     `json:"proxy_image,omitempty"`
	Bind                  string                     `json:"bind,omitempty"`
	CandidateFrontend     string                     `json:"candidate_frontend,omitempty"`
	DrainStartedAt        string                     `json:"drain_started_at,omitempty"`
	ResolvedImage         string                     `json:"resolved_image,omitempty"`
	HandoffStatus         string                     `json:"handoff_status,omitempty"`
	Format                int                        `json:"format,omitempty"`
	Port                  int                        `json:"port,omitempty"`
	Tunnel                bool                       `json:"tunnel,omitempty"`
	ReleasePaused         bool                       `json:"release_paused,omitempty"`
	Colors                object                     `json:"colors"`
	InitialContract       object                     `json:"initial_contract,omitempty"`
	ApplicationEnv        object                     `json:"application_env,omitempty"`
	DatabaseEnv           object                     `json:"database_env,omitempty"`
	Schema                object                     `json:"schema,omitempty"`
	YuactionSchema        object                     `json:"yuaction_schema,omitempty"`
	ReconciliationRestore object                     `json:"reconciliation_restore,omitempty"`
	Extra                 map[string]json.RawMessage `json:"-"`
}

type stateJSON deploymentState

func (s *deploymentState) UnmarshalJSON(data []byte) error {
	var known stateJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&known); err != nil {
		return fmt.Errorf("invalid deployment state: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// Include zero values in this key set, including absent optional fields.
	for _, key := range []string{"prefix", "role", "phase", "target", "network", "active", "previous", "initial_image", "database_network", "database_id", "database_image", "database_volume", "application_volume", "legacy_id", "legacy_restart", "legacy_frontend", "proxy_image", "bind", "candidate_frontend", "drain_started_at", "resolved_image", "handoff_status", "colors", "initial_contract", "application_env", "database_env", "schema", "yuaction_schema", "reconciliation_restore", "format", "port", "tunnel", "release_paused"} {
		delete(raw, key)
	}
	known.Extra = raw
	if known.Colors == nil {
		known.Colors = object{}
	}
	*s = deploymentState(known)
	return s.validate()
}
func (s *deploymentState) MarshalJSON() ([]byte, error) {
	known, err := json.Marshal((*stateJSON)(s))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(known, &fields); err != nil {
		return nil, err
	}
	for k, v := range s.Extra {
		if _, exists := fields[k]; !exists {
			fields[k] = v
		}
	}
	return json.Marshal(fields)
}
func (s *deploymentState) validate() error {
	if s.Format != 1 {
		return fmt.Errorf("unsupported state format; conversion required")
	}
	switch s.Role {
	case "", "main", "edge", "yuaction":
	default:
		return fmt.Errorf("unsupported installation role")
	}
	switch s.Phase {
	case "initializing", "importing", "candidate", "switching", "observing", "draining", "ready", "uninstalled":
	default:
		return fmt.Errorf("invalid deployment phase")
	}
	for _, color := range []string{s.Active, s.Previous, s.Target} {
		if color != "" && color != "blue" && color != "green" {
			return fmt.Errorf("invalid deployment color")
		}
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("invalid entry port")
	}
	for _, values := range []object{s.ApplicationEnv, s.DatabaseEnv, s.Schema, s.YuactionSchema, s.ReconciliationRestore} {
		for _, value := range values {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("invalid string in deployment configuration")
			}
		}
	}
	if s.InitialContract != nil {
		var initial releaseContract
		if err := json.Unmarshal(marshal(s.InitialContract), &initial); err != nil {
			return fmt.Errorf("invalid initial release contract: %w", err)
		}
		if err := initial.validate(); err != nil {
			return err
		}
	}
	for color, value := range s.Colors {
		if color != "blue" && color != "green" {
			return fmt.Errorf("invalid recorded color")
		}
		release, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid color release")
		}
		var typed colorRelease
		if err := json.Unmarshal(marshal(release), &typed); err != nil {
			return fmt.Errorf("invalid color release: %w", err)
		}
		if typed.Image == "" {
			return fmt.Errorf("recorded release lacks image")
		}
		if err := typed.Contract.validate(); err != nil {
			return err
		}
	}
	return nil
}
func readDeploymentState(path string) *deploymentState {
	//nolint:gosec // Operator-selected protected local lifecycle state.
	data, err := os.ReadFile(path)
	check(err, "cannot read protected deployment state")
	var state deploymentState
	check(json.Unmarshal(data, &state), "invalid or unsupported deployment state")
	return &state
}
func stateFromObject(value object) *deploymentState {
	var state deploymentState
	err := json.Unmarshal(marshal(value), &state)
	if err != nil {
		fail(err.Error())
	}
	return &state
}

// Only the explicit status/field interface serializes typed fields dynamically.
func (s *deploymentState) publicValue(key string) any { return obj(decode(marshal(s)))[key] }

type colorRelease struct {
	Image          string            `json:"image"`
	FrontendImage  string            `json:"frontend_image,omitempty"`
	EmptySpool     bool              `json:"empty_spool,omitempty"`
	ApplicationEnv map[string]string `json:"application_env,omitempty"`
	Contract       releaseContract   `json:"contract"`
}

// Decode compatibility promises strictly: booleans/fractional numbers may not
// silently become zero, and unknown future fields remain in the source map.
type releaseContract struct {
	Protocol            int               `json:"protocol"`
	StateEpoch          int               `json:"state_epoch"`
	EdgeProtocolMin     int               `json:"edge_protocol_min"`
	EdgeProtocolMax     int               `json:"edge_protocol_max"`
	ProviderCredentials int               `json:"provider_credentials"`
	EdgeConfiguration   int               `json:"edge_configuration"`
	UpgradeWorkflow     int               `json:"upgrade_workflow"`
	ContainerMemoryMB   int               `json:"container_memory_mb"`
	MinimumFreeMemoryMB int               `json:"minimum_free_memory_mb"`
	ExpandMigrations    []string          `json:"expand_migrations"`
	Component           string            `json:"component"`
	RecordingHandoff    int               `json:"recording_handoff"`
	Schema              map[string]string `json:"schema"`
}

func (c *releaseContract) validate() error {
	if c.Protocol != 1 || c.StateEpoch != 1 {
		return fmt.Errorf("unsupported release/state protocol")
	}
	if c.ExpandMigrations == nil {
		return fmt.Errorf("release lacks reviewed expand-only migration manifest")
	}
	lo, hi := c.EdgeProtocolMin, c.EdgeProtocolMax
	if lo == 0 {
		lo = 1
	}
	if hi == 0 {
		hi = 1
	}
	if lo < 1 || hi < lo {
		return fmt.Errorf("invalid Edge protocol range")
	}
	if c.ContainerMemoryMB < 0 || c.MinimumFreeMemoryMB < 0 || c.ProviderCredentials < 0 || c.EdgeConfiguration < 0 || c.UpgradeWorkflow < 0 {
		return fmt.Errorf("invalid release capability")
	}
	return nil
}
func validateReleaseContract(value object) {
	var contract releaseContract
	err := json.Unmarshal(marshal(value), &contract)
	if err != nil {
		fail("invalid release contract: " + err.Error())
	}
	if err = contract.validate(); err != nil {
		fail(err.Error())
	}
}
