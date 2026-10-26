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
	// authFileDigestLen 是凭证文件名里保留的 key 摘要长度（hex）。12 位足以
	// 区分同一个 label 下的多把 key，又不会把文件名变成一串乱码。
	authFileDigestLen = 12
	// managedAuthStagingSuffix stages a removal under a name the host's auth
	// watcher ignores (it does not end in .json), so a failed unlink cannot
	// resurrect the credential.
	managedAuthStagingSuffix = ".reconcile-removing-"
)

func authRecordID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return managedAuthPrefix + hex.EncodeToString(digest[:])
}

// authFileName returns the readable base name of a credential file:
// <Label>-<sha256 prefix>. The panel lists credentials by file name, so a
// human-readable label plus a short digest reads far better than the 64-hex
// canonical id, while the digest still proves this plugin created the file.
func authFileName(label, key string) string {
	digest := sha256.Sum256([]byte(key))
	return sanitizeAuthLabel(label) + "-" + hex.EncodeToString(digest[:])[:authFileDigestLen]
}

// hasManagedDigestSuffix reports whether base ends in the short key digest for
// this key, which is how reconcile proves ownership after a label change.
func hasManagedDigestSuffix(base, key string) bool {
	digest := sha256.Sum256([]byte(key))
	return strings.HasSuffix(base, "-"+hex.EncodeToString(digest[:])[:authFileDigestLen])
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
		canonical := hasManagedDigestSuffix(base, record.APIKey)
		if !canonical && !isLegacyManagedName(base) {
			// A hand-written record with a custom file name: never touched.
			debugTrace("auth reconcile skipped file=%s reason=unmanaged-name", filepath.Base(path))
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
		wantBase := authFileName(accountLabel(cfg, record.APIKey, record.Label, 0), record.APIKey)
		if base != wantBase {
			// Older versions wrote the 64-hex canonical id or a bare readable
			// label; fold both into <Label>-<short digest> so the panel shows a
			// human name. The JSON (and its id) is untouched, so the auth index
			// and usage history stay stable.
			if err := adoptManagedAuthFile(path, wantBase); err != nil {
				_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not adopted: %v", shortAuthID(id), err), nil)
				continue
			}
			_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s renamed to %s.json", shortAuthID(id), wantBase), nil)
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

// isLegacyManagedName reports whether a file name was generated by an older
// version of this plugin: the 64-hex canonical id
// ("opencode-go-key-<digest>.json") or a bare readable label
// ("OpenCode-Go.json", "OpenCode Go 2.json"). Only such records may be
// adopted or removed without a digest-matching suffix; custom names are
// treated as hand-written and left alone.
func isLegacyManagedName(base string) bool {
	lower := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(base), " ", "-"))
	return strings.HasPrefix(lower, "opencode-go")
}

// adoptManagedAuthFile renames a managed record to its current canonical base
// name (<Label>-<short digest>). The JSON (and the auth id inside it) is not
// rewritten, so the host's auth index is preserved. When the target already
// exists the source is a duplicate and is removed.
func adoptManagedAuthFile(path, base string) error {
	target := filepath.Join(filepath.Dir(path), base+".json")
	if filepath.Clean(path) == filepath.Clean(target) {
		return nil
	}
	if _, err := os.Stat(target); err == nil {
		return removeManagedAuthFile(path)
	}
	return os.Rename(path, target)
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
