package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// isolate points the settings file at a temp path so real user settings
// are never touched.
func isolate(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("HWCLOUD_CLI_CONFIG", p)
	return p
}

// UpdateSettings preserves unknown / unrelated fields.
func TestUpdateSettingsPreservesOtherFields(t *testing.T) {
	p := isolate(t)
	existing := map[string]any{
		"provider":            map[string]any{"openai": map[string]any{"api_key": "sk-old"}},
		"unknown_future_field": map[string]any{"keep": "me"},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := UpdateSettings(func(raw map[string]json.RawMessage) error {
		raw["channels"], _ = json.Marshal(map[string]any{"feishu": map[string]string{"app_id": "cli_new"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings not valid JSON: %v", err)
	}
	var unknown map[string]string
	if err := json.Unmarshal(got["unknown_future_field"], &unknown); err != nil {
		t.Fatalf("unknown field mangled: %s", got["unknown_future_field"])
	}
	if unknown["keep"] != "me" {
		t.Fatalf("unknown field content lost: %+v", unknown)
	}
	if !json.Valid(got["provider"]) {
		t.Fatalf("provider mangled: %s", got["provider"])
	}
	if string(got["channels"]) == "" {
		t.Fatal("channels not written")
	}
}

// A failing fn aborts the update (nothing written).
func TestUpdateSettingsAbortsOnError(t *testing.T) {
	p := isolate(t)
	if err := UpdateSettings(func(raw map[string]json.RawMessage) error {
		raw["channels"], _ = json.Marshal("x")
		return os.ErrInvalid
	}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("settings file created despite aborted update: %v", err)
	}
}

// Concurrent updates must not lose each other's fields (serialized
// read-modify-write) and must leave valid JSON.
func TestUpdateSettingsConcurrent(t *testing.T) {
	p := isolate(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "field" + string(rune('a'+i))
			if err := UpdateSettings(func(raw map[string]json.RawMessage) error {
				raw[key], _ = json.Marshal(i)
				return nil
			}); err != nil {
				t.Errorf("update %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings corrupt after concurrent updates: %v", err)
	}
	// All eight fields landed (none lost to a read-modify-write race).
	for i := 0; i < 8; i++ {
		key := "field" + string(rune('a'+i))
		if _, ok := got[key]; !ok {
			t.Fatalf("field %s lost in concurrent updates", key)
		}
	}
}
