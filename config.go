package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// cfgMu serializes every load→mutate→save cycle on the shared config file (see
// updateConfig). Whole-file saves are not composable, so concurrent writers must
// funnel through it.
var cfgMu sync.Mutex

// updateConfig serializes a load→mutate→save cycle on the shared config file.
// Every writer must go through it: the doctor loop and the Telegram update
// handlers mutate the config concurrently (profiles, model), and racing
// whole-file saves silently drop each other's changes.
// mutate runs on a freshly-loaded copy; return true to persist it. Returns the
// fresh (possibly mutated) config, or nil if the config could not be loaded.
func updateConfig(mutate func(*Config) bool) *Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}
	if mutate(config) {
		saveConfig(config) // safe-ignore: best-effort persist under the lock; caller keeps the fresh copy either way
	}
	return config
}

// configDir returns ~/.config/ccc (created if needed)
func configDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "ccc")
	os.MkdirAll(dir, 0755)
	return dir
}

// cacheDir returns ~/Library/Caches/ccc (created if needed)
func cacheDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "Caches", "ccc")
	os.MkdirAll(dir, 0755)
	return dir
}

func getConfigPath() string {
	// Migrate from old path if needed
	home, _ := os.UserHomeDir()
	oldPath := filepath.Join(home, ".ccc.json")
	newPath := filepath.Join(configDir(), "config.json")
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if _, err := os.Stat(oldPath); err == nil {
			data, _ := os.ReadFile(oldPath)
			os.WriteFile(newPath, data, 0600)
			os.Remove(oldPath)
		}
	}
	return newPath
}

// loadConfig reads <config_dir>/config.json. Keys ccc no longer knows (the v2
// `sessions` map above all) are simply ignored: v3 keeps no session state in
// this file, and the first `saveConfig` drops them for good.
func loadConfig() (*Config, error) {
	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return nil, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// saveConfig writes the config atomically: it marshals to a temp file in the
// same directory and renames it over config.json, so hook processes (and other
// readers) never observe a torn/partial write.
func saveConfig(config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	path := getConfigPath()
	tmp, err := os.CreateTemp(filepath.Dir(path), "config.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()        // safe-ignore: best-effort cleanup on an error path
		os.Remove(tmpName) // safe-ignore: best-effort cleanup on an error path
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()        // safe-ignore: best-effort cleanup on an error path
		os.Remove(tmpName) // safe-ignore: best-effort cleanup on an error path
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) // safe-ignore: best-effort cleanup on an error path
		return err
	}
	return os.Rename(tmpName, path)
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
