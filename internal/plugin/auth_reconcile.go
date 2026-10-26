package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/config"
)

const (
	// managedAuthPrefix names the records this plugin materializes from
	// cfg.APIKeys: <prefix><sha256(api_key)>.json. The digest is non-secret
	// and independent of config ordering, so it doubles as the ownership
	// proof when reconciling.
	managedAuthPrefix = "opencode-go-key-"
	// managedAuthStagingSuffix stages a removal under a name the host's auth
	// watcher ignores (it does not end in .json), so a failed unlink cannot
	// resurrect the credential.
	managedAuthStagingSuffix = ".reconcile-removing-"
)

func authRecordID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return managedAuthPrefix + hex.EncodeToString(digest[:])
}

// reconcileAuthRecords makes the host's credential list track cfg.APIKeys:
// configured keys without a record are created, managed records for removed
// keys are deleted, and a managed record for a configured key is re-enabled.
//
// Only records this plugin can prove it created are touched: the file name
// must be the canonical managed name AND the api_key inside must hash to the
// digest in that name. Console OAuth records and hand-written files are never
// deleted here.
func (m *Manager) reconcileAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil {
		return nil
	}
	if err := m.materializeAuthRecords(ctx, cfg); err != nil {
		return err
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		// Listing is best effort: creation already succeeded, and failing
		// registration would take the whole provider down.
		return nil
	}
	expected := make(map[string]string, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		expected[authRecordID(key.Value)] = key.Value
	}
	for _, entry := range entries {
		if entry.RuntimeOnly {
			continue
		}
		if entry.Type != ProviderID && entry.Provider != ProviderID {
			continue
		}
		path := strings.TrimSpace(entry.Path)
		base := strings.TrimSuffix(filepath.Base(path), ".json")
		if path == "" || !strings.HasPrefix(base, managedAuthPrefix) {
			continue
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var record authRecord
		if json.Unmarshal(data, &record) != nil {
			continue
		}
		if record.isOAuth() || strings.TrimSpace(record.APIKey) == "" {
			continue
		}
		id := authRecordID(record.APIKey)
		if id != base {
			// The file name and the key disagree: not provably ours.
			debugTrace("auth reconcile skipped id=%s file=%s reason=digest-mismatch", shortAuthID(id), filepath.Base(path))
			continue
		}
		if _, configured := expected[id]; !configured {
			if err := removeManagedAuthFile(path); err != nil {
				_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not removed: %v", shortAuthID(id), err), nil)
				continue
			}
			_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s removed: no longer listed in configured api-keys", shortAuthID(id)), nil)
			continue
		}
		if !entry.Disabled && !recordDisabled(data) {
			continue
		}
		enabled, err := setAuthRecordDisabled(data, false)
		if err != nil {
			continue
		}
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: entry.Name, JSON: enabled}); err != nil {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not re-enabled: %v", shortAuthID(id), err), nil)
			continue
		}
		_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s re-enabled: it is listed in configured api-keys", shortAuthID(id)), nil)
	}
	return nil
}

// removeManagedAuthFile unlinks a proven-managed credential through a staging
// name, so a partially failed removal never leaves a loadable .json behind.
func removeManagedAuthFile(path string) error {
	staged := path + managedAuthStagingSuffix
	if err := os.Rename(path, staged); err != nil {
		return err
	}
	if err := os.Remove(staged); err != nil {
		// Best effort restore: the original name is what the host loads.
		_ = os.Rename(staged, path)
		return err
	}
	return nil
}

func recordDisabled(data []byte) bool {
	var raw struct {
		Disabled bool `json:"disabled"`
	}
	return json.Unmarshal(data, &raw) == nil && raw.Disabled
}

// setAuthRecordDisabled rewrites only the disabled flag, preserving every
// other field (labels, ids and future host-managed keys).
func setAuthRecordDisabled(data []byte, disabled bool) ([]byte, error) {
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	record["disabled"] = disabled
	return json.Marshal(record)
}

func shortAuthID(id string) string {
	const prefix = "opencode-go-key-"
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix)+8 {
		return id[:len(prefix)+8]
	}
	if len(id) > 16 {
		return id[:16]
	}
	return id
}
