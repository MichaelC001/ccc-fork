package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func installService() error {
	home, _ := os.UserHomeDir()

	// Detect OS and install appropriate service
	if _, err := os.Stat("/Library"); err == nil {
		// macOS - use launchd
		return installLaunchdService(home)
	}
	// Linux - use systemd
	return installSystemdService(home)
}

func installLaunchdService(home string) error {
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents dir: %w", err)
	}

	plistPath := filepath.Join(plistDir, "com.ccc.plist")
	logPath := filepath.Join(cacheDir(), "ccc.log")

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.ccc</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>listen</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, cccPath, logPath, logPath)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("failed to write plist: %w", err)
	}

	// Unload if exists, then load
	exec.Command("launchctl", "unload", plistPath).Run()
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("failed to load service: %w", err)
	}

	fmt.Println("✅ Service installed and started (launchd)")
	return nil
}

// installSystemdService writes a systemd USER unit. User rather than system:
// the profiles, the data dir and the credentials all live in the owner's home,
// and `claude` resolves them from $HOME. Enable lingering
// (`loginctl enable-linger <user>`) so it survives logout on a VM.
func installSystemdService(home string) error {
	serviceDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return fmt.Errorf("failed to create systemd dir: %w", err)
	}

	servicePath := filepath.Join(serviceDir, "ccc.service")
	service := renderSystemdUnit(cccPath, loadConfigOrNil())
	if err := os.WriteFile(servicePath, []byte(service), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	exec.Command("systemctl", "--user", "daemon-reload").Run() // safe-ignore: a failure surfaces on the start below
	exec.Command("systemctl", "--user", "enable", "ccc").Run() // safe-ignore: same
	if err := exec.Command("systemctl", "--user", "start", "ccc").Run(); err != nil {
		return fmt.Errorf("failed to start service: %w (is XDG_RUNTIME_DIR set? see the README)", err)
	}

	fmt.Printf("✅ Service installed and started (systemd --user): %s\n", servicePath)
	fmt.Println("   Make it survive logout: loginctl enable-linger $USER")
	return nil
}

// renderSystemdUnit builds the unit text. The only Environment= lines it emits
// are the env_passthrough names that are actually set right now: those are the
// secrets bots are meant to inherit (DESIGN §3.1), and systemd gives the
// service a bare environment otherwise. Everything else a bot may see is built
// by claudeEnv at spawn time, not here.
func renderSystemdUnit(binary string, config *Config) string {
	var env strings.Builder
	if config != nil {
		for _, name := range config.EnvPassthrough {
			name = strings.TrimSpace(name)
			if name == "" || strings.HasPrefix(name, "CLAUDE") || strings.HasPrefix(name, "ANTHROPIC") {
				continue
			}
			value, ok := os.LookupEnv(name)
			if !ok {
				continue
			}
			// systemd quoting: one "NAME=value" per line, value in quotes with
			// backslashes and quotes escaped.
			escaped := strings.ReplaceAll(value, `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `"`, `\"`)
			fmt.Fprintf(&env, "Environment=\"%s=%s\"\n", name, escaped)
		}
	}
	return fmt.Sprintf(`[Unit]
Description=ccc - a team of Claude bots in one Telegram forum group
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s listen
Restart=always
RestartSec=10
KillMode=mixed
TimeoutStopSec=30
%s
[Install]
WantedBy=default.target
`, binary, env.String())
}
