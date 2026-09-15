package main

import "testing"

const capturedCodexDeviceScreen = `
Welcome to Codex [v0.154.0]
OpenAI's command-line coding agent

Follow these steps to sign in with ChatGPT using device code authorization:

1. Open this link in your browser and sign in to your account
   https://auth.openai.com/codex/device

2. Enter this one-time code (expires in 15 minutes)
   EVA2-V4DSJ

Continue only if you started this login in Codex. If a website or another person gave you this code, cancel.
`

func TestExtractDeviceLoginCodexScreen(t *testing.T) {
	url, code := extractDeviceLogin(capturedCodexDeviceScreen)
	if url != "https://auth.openai.com/codex/device" {
		t.Errorf("url = %q", url)
	}
	if code != "EVA2-V4DSJ" {
		t.Errorf("code = %q, want the full 4-5 Codex user_code (not truncated to 4-4)", code)
	}
}

func TestExtractDeviceLoginGrokStyleCode(t *testing.T) {
	screen := "Visit https://example.com/device and enter ABCD-EFGH"
	url, code := extractDeviceLogin(screen)
	if url != "https://example.com/device" {
		t.Errorf("url = %q", url)
	}
	if code != "ABCD-EFGH" {
		t.Errorf("code = %q", code)
	}
}
