package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/config"
)

// legacyRecorder serves auth.list from a fixed entry set and records saves.
type legacyRecorder struct {
	entries []pluginapi.HostAuthFileEntry
	saves   []pluginapi.HostAuthSaveRequest
}

func (l *legacyRecorder) responder() func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			files := make([]map[string]any, 0, len(l.entries))
			for _, e := range l.entries {
				files = append(files, map[string]any{
					"id": e.ID, "name": e.Name, "type": e.Type, "provider": e.Provider,
					"path": e.Path, "disabled": e.Disabled,
				})
			}
			return hostOK(map[string]any{"files": files}), nil
		case pluginabi.MethodHostAuthSave:
			var req pluginapi.HostAuthSaveRequest
			_ = json.Unmarshal(payload, &req)
			l.saves = append(l.saves, req)
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostLog:
			return hostOK(map[string]any{}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	}
}

func writeKeyRecord(t *testing.T, path, key string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"type": ProviderID, "api_key": key})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

func TestReconcileAdoptsLegacyFileNameThenRemovesIt(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "OpenCode-Go.json")
	const key = "sk-legacy-adopt"
	writeKeyRecord(t, legacy, key)

	rec := &legacyRecorder{entries: []pluginapi.HostAuthFileEntry{{
		ID: "OpenCode-Go.json", Name: "OpenCode-Go.json", Type: ProviderID, Provider: ProviderID, Path: legacy,
	}}}
	m := NewManager(NewHostBridge(rec.responder()))
	cfg, err := config.Load([]byte("api-keys:\n  - value: \"" + key + "\"\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := m.reconcileAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	canonical := filepath.Join(dir, authRecordID(key)+".json")
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("legacy record was not adopted: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file still present: %v", err)
	}

	// Removing the key from the config must now delete the record: its name
	// carries the digest, so ownership is provable.
	rec.entries = []pluginapi.HostAuthFileEntry{{
		ID: authRecordID(key), Name: authRecordID(key) + ".json", Type: ProviderID, Provider: ProviderID, Path: canonical,
	}}
	empty := cfg
	empty.APIKeys = nil
	if err := m.reconcileAuthRecords(context.Background(), empty); err != nil {
		t.Fatalf("reconcile after removal: %v", err)
	}
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("canonical record survived key removal: %v", err)
	}
}

func TestReconcileLeavesCustomFileNamesAlone(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "My-Laptop-Key.json")
	writeKeyRecord(t, custom, "sk-custom-name")
	rec := &legacyRecorder{entries: []pluginapi.HostAuthFileEntry{{
		ID: "custom", Name: "My-Laptop-Key.json", Type: ProviderID, Provider: ProviderID, Path: custom,
	}}}
	m := NewManager(NewHostBridge(rec.responder()))
	cfg, err := config.Load([]byte("api-keys:\n  - value: \"sk-other\"\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := m.reconcileAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("hand-written record must not be touched: %v", err)
	}
}

func TestReconcileReenablesConfiguredRecord(t *testing.T) {
	dir := t.TempDir()
	const key = "sk-disabled-one"
	id := authRecordID(key)
	path := filepath.Join(dir, id+".json")
	body, _ := json.Marshal(map[string]any{"type": ProviderID, "api_key": key, "disabled": true})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rec := &legacyRecorder{entries: []pluginapi.HostAuthFileEntry{{
		ID: id, Name: id + ".json", Type: ProviderID, Provider: ProviderID, Path: path, Disabled: true,
	}}}
	m := NewManager(NewHostBridge(rec.responder()))
	cfg, err := config.Load([]byte("api-keys:\n  - value: \"" + key + "\"\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := m.reconcileAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var found bool
	for _, save := range rec.saves {
		if save.Name != id+".json" {
			continue
		}
		var record map[string]any
		if json.Unmarshal(save.JSON, &record) != nil {
			t.Fatalf("save payload: %s", save.JSON)
		}
		if record["disabled"] != false {
			t.Fatalf("disabled flag not cleared: %s", save.JSON)
		}
		found = true
	}
	if !found {
		t.Fatalf("record was not re-enabled: %+v", rec.saves)
	}
}
