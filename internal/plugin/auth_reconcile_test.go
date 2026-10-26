package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestReconcileKeepsReadableFileNameAndRemovesUnknownKey(t *testing.T) {
	dir := t.TempDir()
	readable := filepath.Join(dir, authFileName("OpenCode Go")+".json")
	const key = "sk-legacy-adopt"
	writeKeyRecord(t, readable, key)

	rec := &legacyRecorder{entries: []pluginapi.HostAuthFileEntry{{
		ID: authRecordID(key), Name: authFileName("OpenCode Go") + ".json", Type: ProviderID, Provider: ProviderID, Path: readable,
	}}}
	m := NewManager(NewHostBridge(rec.responder()))
	cfg, err := config.Load([]byte("api-keys:\n  - value: \"" + key + "\"\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := m.reconcileAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(readable); err != nil {
		t.Fatalf("readable record must stay untouched: %v", err)
	}

	// Removing the key from the config must delete the record: its api_key is
	// no longer configured and its file name is one this plugin writes.
	empty := cfg
	empty.APIKeys = nil
	if err := m.reconcileAuthRecords(context.Background(), empty); err != nil {
		t.Fatalf("reconcile after removal: %v", err)
	}
	if _, err := os.Stat(readable); !os.IsNotExist(err) {
		t.Fatalf("record survived key removal: %v", err)
	}
}

func TestReconcileRenamesHashedFileNameToReadable(t *testing.T) {
	dir := t.TempDir()
	hashed := filepath.Join(dir, authRecordID("hashed-key-value")+".json")
	writeKeyRecord(t, hashed, "hashed-key-value")
	rec := &legacyRecorder{entries: []pluginapi.HostAuthFileEntry{{
		ID: authRecordID("hashed-key-value"), Name: filepath.Base(hashed), Type: ProviderID, Provider: ProviderID, Path: hashed,
	}}}
	m := NewManager(NewHostBridge(rec.responder()))
	cfg, err := config.Load([]byte("api-keys:\n  - value: hashed-key-value\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := m.reconcileAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	readable := filepath.Join(dir, authFileName("OpenCode Go")+".json")
	if _, err := os.Stat(readable); err != nil {
		t.Fatalf("hashed name was not folded back: %v", err)
	}
	if _, err := os.Stat(hashed); !os.IsNotExist(err) {
		t.Fatalf("hashed file still present: %v", err)
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
	fileBase := authFileName("OpenCode Go")
	path := filepath.Join(dir, fileBase+".json")
	body, _ := json.Marshal(map[string]any{"type": ProviderID, "api_key": key, "disabled": true})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rec := &legacyRecorder{entries: []pluginapi.HostAuthFileEntry{{
		ID: id, Name: fileBase + ".json", Type: ProviderID, Provider: ProviderID, Path: path, Disabled: true,
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
		if save.Name != fileBase+".json" {
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

func TestAuthFileNameIsReadableAndStable(t *testing.T) {
	if got := authFileName("OpenCode Go"); got != "OpenCode-Go" {
		t.Fatalf("file name is not the readable label: %q", got)
	}
	if got := authFileName("Work"); got != "Work" {
		t.Fatalf("label not honored: %q", got)
	}
	if got := authFileName(""); got != "OpenCode-Go" {
		t.Fatalf("empty label fallback: %q", got)
	}
	if got := authFileName("工作 账号/2"); strings.ContainsAny(got, " /") {
		t.Fatalf("unsafe characters survived: %q", got)
	}
	// A second account with the same label is disambiguated by a suffix, not
	// by a key digest: the name must never depend on the key.
	existing := map[string]struct{}{"OpenCode-Go.json": {}}
	if got := availableAuthName("OpenCode Go", existing); got != "OpenCode-Go-2.json" {
		t.Fatalf("duplicate label was not disambiguated: %q", got)
	}
}
