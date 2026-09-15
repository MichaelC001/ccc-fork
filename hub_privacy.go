package main

import (
	"net/http"
	"strings"
)

func serveHubPrivacy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path != "/privacy" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(hubPrivacyHTML))
}

const hubPrivacyHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Privacy — CCC</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 16px/1.5 ui-sans-serif, system-ui, sans-serif; max-width: 40rem; margin: 2rem auto; padding: 0 1.25rem 4rem; }
  h1 { font-size: 1.6rem; }
  h2 { font-size: 1.15rem; margin-top: 2rem; }
  a { color: inherit; }
  .meta { color: #666; }
  @media (prefers-color-scheme: dark) { .meta { color: #9aa; } }
</style>
</head>
<body>
<h1>Privacy policy — CCC</h1>
<p class="meta">Last updated: 2026-09-15 · Phone app: <a href="https://github.com/kidandcat/ccc-app">ccc-app</a> · Server: <a href="https://github.com/kidandcat/ccc">ccc</a></p>

<p>CCC (Crew Command Center) is an open-source phone client for <em>your</em> <code>ccc listen</code> instance. There is no CCC user account.</p>

<h2>What the app stores on the device</h2>
<ul>
  <li>A Curve25519 keypair (to encrypt traffic to your machines)</li>
  <li>The list of machines you paired (hub URL, public key, name)</li>
</ul>
<p>Nothing else is persisted. Uninstalling the app, or removing a machine in the app, deletes that data.</p>

<h2>What the public hub sees</h2>
<p>This host (<code>hub.mentasystems.com</code>) is an encrypted relay. It sees:</p>
<ul>
  <li>Public keys and short-lived pairing codes</li>
  <li>Opaque ciphertext (NaCl boxes)</li>
</ul>
<p>It does <strong>not</strong> see your Telegram token, bot prompts, chat text, files, or engine credentials. Anyone may run their own hub (<code>ccc hub</code>) and point <code>ccc config set hub_url</code> at it.</p>

<h2>What your machine sees</h2>
<p>Your <code>ccc listen</code> process receives the messages you send from the app, the same way it receives Telegram messages. That data stays on the machine you control.</p>

<h2>No tracking</h2>
<p>The app does not include analytics SDKs, advertising identifiers, or crash reporters that phone home. This hub does not set cookies.</p>

<h2>Children</h2>
<p>The app is a developer tool. It is not directed at children.</p>

<h2>Contact</h2>
<p>Issues: <a href="https://github.com/kidandcat/ccc/issues">github.com/kidandcat/ccc/issues</a><br>
Email: <a href="mailto:kidandcat@gmail.com">kidandcat@gmail.com</a></p>

<h2 id="delete">Deleting data</h2>
<p>There is no cloud account to delete. In the app, long-press a machine to unpair it. On the machine, <code>ccc unpair</code> revokes the device. Uninstalling the app removes local keys. Pairing codes on this hub expire after 10 minutes.</p>
</body>
</html>
`
