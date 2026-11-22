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
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/config"
)

const (
	// managedAuthPrefix 是插件内部 auth id 的前缀（写进 JSON 的 id 字段）：
	// <prefix><sha256(api_key)>。id 与文件名解耦，保持稳定，CPA 的 auth_index
	// 和 Keeper 的历史统计不会因为改文件名而漂移。
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

// authFileName returns the readable base name of a credential file: the plain
// account label ("OpenCode-Go"). Ownership is proven by the api_key inside the
// file, not by the name, so the name carries no digest at all. This keeps the
// host auth index stable, because that index is derived from the file path.
func authFileName(label string) string {
	return sanitizeAuthLabel(label)
}

// sanitizeAuthLabel folds a display label into a file-name-safe stem.
func sanitizeAuthLabel(label string) string {
	base := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.TrimSpace(label))
	base = strings.Trim(base, "-")
	if runes := []rune(base); len(runes) > 60 {
		base = strings.Trim(string(runes[:60]), "-")
	}
	if base == "" {
		base = "OpenCode-Go"
	}
	return base
}

// reconcileAuthRecords makes the host's credential list track cfg.APIKeys:
// configured keys without a record are created, managed records for removed
// keys are deleted, and a managed record for a configured key is re-enabled.
//
// Only records this plugin can prove it created are touched: the file name
// must carry the short key digest (or be a legacy managed name) AND the
// api_key inside must hash to a configured key. Hand-written files are never
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
	for _, entry := range cfg.KeyEntries() {
		expected[authRecordID(entry.Key.Value)] = entry.Upstream
	}
	for _, entry := range entries {
		if entry.RuntimeOnly {
			continue
		}
		if entry.Type != ProviderID && entry.Provider != ProviderID {
			continue
		}
		path := strings.TrimSpace(entry.Path)
		if path == "" {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(path), ".json")
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var record authRecord
		if json.Unmarshal(data, &record) != nil {
			continue
		}
		if strings.TrimSpace(record.APIKey) == "" {
			continue
		}
		id := authRecordID(record.APIKey)
		upstream, configured := expected[id]
		if !configured && !managedAuthRecord(base, record) {
			// The api_key is not one of ours and the file was not written by
			// this plugin: a hand-written record, never touched.
			debugTrace("auth reconcile skipped file=%s reason=unmanaged-key", filepath.Base(path))
			continue
		}
		if !configured {
			if err := removeManagedAuthFile(path); err != nil {
				_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not removed: %v", shortAuthID(id), err), nil)
				continue
			}
			_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s removed: no longer listed in configured api-keys", shortAuthID(id)), nil)
			continue
		}
		recordUpstream := resolvedUpstreamName(cfg, record.Upstream)
		if configured && strings.TrimSpace(upstream) != "" {
			recordUpstream = upstream
		}
		wantBase := authFileName(accountLabel(cfg, recordUpstream, record.APIKey, record.Label, 0))
		renamed := false
		if base != wantBase {
			// Older versions wrote the 64-hex canonical id; fold those into the
			// readable label. The JSON (and its id) is not rewritten, so the
			// host's auth index and usage history stay stable.
			newPath, err := adoptManagedAuthFile(path, wantBase, expected)
			if err != nil {
				_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not adopted: %v", shortAuthID(id), err), nil)
				continue
			}
			if filepath.Clean(newPath) != filepath.Clean(path) {
				_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s renamed to %s", shortAuthID(id), filepath.Base(newPath)), nil)
				path = newPath
				renamed = true
			}
		}
		if !entry.Disabled && !recordDisabled(data) {
			continue
		}
		enabled, err := setAuthRecordDisabled(data, false)
		if err != nil {
			continue
		}
		saveName := entry.Name
		if renamed {
			saveName = filepath.Base(path)
		}
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: saveName, JSON: enabled}); err != nil {
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

// isLegacyManagedName reports whether a file name was generated by this or an
// older version of the plugin: a readable label ("OpenCode-Go.json",
// "OpenCode Go 2.json") or the 64-hex canonical id
// ("opencode-go-key-<digest>.json"). Such a record is removable even when its
// api_key is no longer configured.
func isLegacyManagedName(base string) bool {
	lower := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(base), " ", "-"))
	return strings.HasPrefix(lower, "opencode-go")
}

// managedAuthRecord reports whether this plugin can prove it wrote the record:
// either the canonical id (derived from the api_key) or a legacy file name this
// plugin used to generate. The readable label alone is not proof — a
// hand-written file may reuse it — so ownership is never inferred from it.
func managedAuthRecord(base string, record authRecord) bool {
	if strings.HasPrefix(strings.TrimSpace(record.ID), managedAuthPrefix) {
		return true
	}
	return isLegacyManagedName(base)
}

// adoptManagedAuthFile renames a managed record to its readable base name and
// returns the resulting path. A record already sitting on the target name is
// only replaced when its key is no longer configured: a stale credential must
// not shadow the configured key that inherited its label. When the target holds
// another configured key (two accounts sharing a label) or a hand-written file,
// both records are kept and the source stays under its disambiguated name.
func adoptManagedAuthFile(path, base string, expected map[string]string) (string, error) {
	target := filepath.Join(filepath.Dir(path), base+".json")
	if filepath.Clean(path) == filepath.Clean(target) {
		return path, nil
	}
	if data, err := os.ReadFile(target); err == nil {
		var occupant authRecord
		if json.Unmarshal(data, &occupant) != nil || strings.TrimSpace(occupant.APIKey) == "" {
			return path, nil
		}
		if _, live := expected[authRecordID(occupant.APIKey)]; live {
			// Another configured key already owns the readable name.
			return path, nil
		}
		if !managedAuthRecord(strings.TrimSuffix(filepath.Base(target), ".json"), occupant) {
			// A hand-written record, never removed from here.
			return path, nil
		}
		if err := removeManagedAuthFile(target); err != nil {
			return "", err
		}
	}
	if err := os.Rename(path, target); err != nil {
		return "", err
	}
	return target, nil
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
