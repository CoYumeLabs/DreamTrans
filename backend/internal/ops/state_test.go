package ops

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTypedStateRejectsMalformedVersionAndCapabilities(t *testing.T) {
	fixture := obj(decode(marshal(testController(t).state)))
	for _, test := range []struct {
		name, key string
		value     any
	}{
		{"future-version", "format", 2}, {"fractional-port", "port", 1.5},
		{"string-port", "port", "8080"}, {"invalid-port", "port", 70000},
		{"invalid-phase", "phase", "guess"}, {"invalid-role", "role", "other"},
		{"invalid-color", "active", "red"}, {"invalid-boolean", "release_paused", "false"},
		{"non-string-environment", "application_env", object{"SECRET": true}},
		{"malformed-colors", "colors", []any{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneObject(fixture)
			candidate[test.key] = test.value
			var state deploymentState
			if err := json.Unmarshal(marshal(candidate), &state); err == nil {
				t.Fatal("malformed journal accepted")
			}
		})
	}
	for _, test := range []struct {
		key   string
		value any
	}{
		{"provider_credentials", true}, {"edge_protocol_min", -1}, {"edge_protocol_max", 0.5},
		{"expand_migrations", []any{17}}, {"minimum_free_memory_mb", -1},
	} {
		t.Run(test.key, func(t *testing.T) {
			candidate := obj(decode(marshal(fixture)))
			contract := obj(obj(obj(candidate["colors"])["blue"])["contract"])
			contract[test.key] = test.value
			var state deploymentState
			if err := json.Unmarshal(marshal(candidate), &state); err == nil {
				t.Fatal("invalid contract accepted")
			}
		})
	}
}

func TestTypedStatePreservesUnknownMetadataAndInitialEmptyColors(t *testing.T) {
	candidate := obj(decode(marshal(testController(t).state)))
	candidate["phase"] = "initializing"
	candidate["active"] = nil
	candidate["previous"] = nil
	candidate["target"] = nil
	candidate["colors"] = object{}
	candidate["future_extension"] = decode([]byte(`{"integer":9007199254740993,"nested":[true,null]}`))
	state := stateFromObject(candidate)
	encoded := marshal(state)
	if !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatal("future integer lost precision")
	}
	var reloaded deploymentState
	if err := json.Unmarshal(encoded, &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Colors == nil {
		t.Fatal("initial colors cannot be resumed")
	}
	reloaded.Colors["blue"] = obj(testController(t).state.Colors["blue"])
	if err := reloaded.validate(); err != nil {
		t.Fatal(err)
	}
}
