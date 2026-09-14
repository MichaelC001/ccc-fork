# ccc - Claude Code Companion

> A team of Claude bots living in one Telegram forum group. Each topic is a bot with its own role, memory and workspace; you talk to it like you talk to a person.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## What ccc is (v3)

A **topic is a bot**, not a session. Each bot has a name, a free-text role you
set with `/role`, its own workspace directory, its own Claude conversation and a
shared persistent memory. You send it a message; it does the work and answers in
the topic. There are no commands in the normal flow.

Under the hood ccc drives Claude Code as a **stateless runner**: every message is
one `claude -p` process with a session id ccc mints and resumes. ccc owns
everything the runner does not — bot identity, memory, inter-bot messaging,
account selection and the Telegram UX.

```
┌────────────┐   message    ┌──────────┐   claude -p --resume  ┌──────────────┐
│  Telegram  │─────────────▶│   ccc    │──────────────────────▶│   one turn   │
│   topic    │◀─────────────│  listen  │◀──  stream-json   ────│              │
└────────────┘   progress   └──────────┘                       └───────┬──────┘
                                  ▲                                    │
                                  │        ccc mcp (stdio)             │
                                  └────────────────────────────────────┘
                       remember / recall / ask_owner / send_to_bot / …
```

- **Turn** — one message (plus a small context envelope of memories and pending
  inbox) becomes one `claude -p` run. One turn per bot at a time; anything you
  send meanwhile is folded into the next turn.
- **Progress** — a single message in the topic, edited in place, showing what the
  bot is doing right now. It is replaced by the answer, and your message gets a
  ✅ when the turn lands.
- **Tools** — the bot talks back to ccc over an MCP server (`ccc mcp`): it can
  `remember` / `recall` / `forget`, `list_bots`, `send_to_bot` (mirrored into
  both topics as 🤝), `notify_owner`, `ask_owner` (inline buttons),
  `update_instructions` and `send_file`.
- **Accounts** — every turn picks the healthiest Claude account (profile); a turn
  that hits a logged-out or rate-limited account is retried on another one.
- **Isolation** — bots load no `CLAUDE.md`, no user/project settings and no
  skills. They run with bypassed permissions in their own workspace.

> The v2 model (one topic = one Claude Code *background agent*, `claude attach`
> handoff, `AskUserQuestion` buttons) is gone. The remaining v2 sections below
> are being rewritten; treat anything mentioning `claude agents` or `--bg` as
> legacy.

## Features

- **A bot per topic** — name, role, workspace and persistent memory
- **Shared memory** — `user`, `project` and per-bot scopes, full-text searchable
- **Bots talk to each other** — `send_to_bot`, mirrored into both topics
- **Questions** — `ask_owner` renders inline buttons; replying also answers
- **Multi-account** — turns are spread across Claude accounts and fail over
- **Voice & images** — transcribed / saved into the bot's `inbox/`
- **File transfer** — `ccc send <file>` (direct, or streaming relay for large files)
- **Self-hosted** — runs entirely on your machine

## Requirements

- macOS or Linux
- Go 1.21+ (to build)
- [Claude Code](https://claude.ai/claude-code) 2.1.259+, logged in, with the bypass-permissions disclaimer accepted once
- A Telegram account + bot

## Install

```bash
git clone https://github.com/kidandcat/ccc.git
cd ccc
make install        # builds, signs (macOS), installs to ~/bin/ccc
ccc --version       # ccc version 2.0.0
```

## Setup

```bash
ccc setup <BOT_TOKEN>
```

This connects to your bot, optionally configures a group with Topics, installs the `ccc-send` skill, and installs + starts the listener service.

For session topics: create a Telegram group with **Topics enabled**, add your bot as **admin**, then run `ccc setgroup` (or send a message in the group during setup).

## Usage

### Terminal

| Command | Description |
|---------|-------------|
| `ccc listen` | Run the instance (bots + Telegram); normally a service |
| `ccc mcp --bot <id> --turn <id>` | The per-turn MCP server. Claude Code spawns it; never run it by hand |
| `ccc send <file>` | Send a file to the current session's topic |
| `ccc doctor` | Check dependencies, profiles and configuration |
| `ccc config` | Show / set configuration |
| `ccc setgroup` | Configure the Telegram group for topics |
| `ccc profile list` | Show Claude accounts, usage and login state |
| `ccc profile add <name> <dir>` | Register a Claude account (`--label <text>`) |
| `ccc profile login <name>` | Log that account in (interactive) |
| `ccc profile default <name>` | Set the account turns prefer |
| `ccc profile remove <name>` | Unregister an unused account |

### Telegram (in your group)

**Send a plain message in General** and ccc creates a new bot: a topic named
after the first line, with that message as its first input. **Send a plain
message in a bot's topic** and that bot runs a turn. That is the whole normal
flow.

| Command | Where | Description |
|---------|-------|-------------|
| `/role [text]` | topic | Show or set this bot's role |
| `/new` | topic | Start a fresh conversation (memories are kept) |
| `/stop` | topic | Kill the running turn and drop the queue |
| `/cwd [path]` | topic | Show or set this bot's working directory |
| `/memory [query]` | topic | List or search the memories this bot can see |
| `/forget <scope> <key>` | topic | Delete one memory |
| `/bot <name> [role]` | group | Create a bot explicitly |
| `/bots` | anywhere | All bots, status and last activity |
| `/status` | anywhere | Accounts, running turns, queue, data dir |

Voice messages are transcribed (voice build) and photos/documents are saved into
the bot's `inbox/` with the path handed to the bot. When a bot calls `ask_owner`
you get inline buttons — tapping one, or replying to the question, answers it and
wakes the bot.

Changing a bot's role (`/role`, or the bot's own `update_instructions`) starts a
fresh conversation: Claude Code records the system prompt once per conversation
and ignores later changes, so a new role needs a new one. Memories survive.

## Profiles (multiple Claude accounts)

ccc runs turns under **several Claude accounts at once**. A profile is just a
`CLAUDE_CONFIG_DIR`: that one variable scopes the account's credentials (the
macOS Keychain service name is derived from the directory), its `.claude.json`,
`projects/`, `jobs/`, `daemon/` and `settings.json`.

The account is chosen **per turn**, not per bot, so a busy account never blocks
a conversation. All profiles of one instance share a `projects/` directory
(symlinked by `ccc listen`), which is what lets a turn that failed on one
account be retried on another and still `--resume` the same conversation.

### Setting one up

```bash
ccc profile add work2 ~/.claude-work2 --label 'me@example.com'
ccc profile login work2                     # interactive; ccc never sees credentials
CLAUDE_CONFIG_DIR=~/.claude-work2 claude --dangerously-skip-permissions   # accept the disclaimer once
ccc profile list
```

```
NAME       LABEL              CONFIG DIR              5h   7d   WORKING  LOGIN
default *  claude's default   /home/you/.claude       7%   42%  2        you@example.com
work2      me@example.com     /home/you/.claude-work2 0%   3%   0        me@example.com
```

The first `ccc profile add` also records your existing account as the `default`
profile, with an **empty** `config_dir` meaning "leave `CLAUDE_CONFIG_DIR`
unset". That is deliberate: setting the variable to `~/.claude` is *not* the
same as leaving it unset — Claude Code then starts a fresh
`~/.claude/.claude.json` instead of using `~/.claude.json`.

### The bypass-permissions disclaimer

Bots run with `--permission-mode bypassPermissions`, which Claude Code gates
behind a disclaimer accepted **once per config dir**. So every new profile needs
one interactive run before ccc can use it:

```
CLAUDE_CONFIG_DIR=~/.claude-work2 claude --dangerously-skip-permissions
```

There is no way to accept it non-interactively, and ccc does not try. `ccc
doctor` reports the state per profile and prints the exact command.

### How a profile is chosen

Each turn goes to the profile with the **lowest five-hour utilization**, ties
broken by fewer working bots and then by name. Utilization comes from
`cachedUsageUtilization` in each profile's `.claude.json` — Claude Code's own
cache, which is often stale and sometimes absent; a missing number counts as
50%. A profile that hits a usage/rate limit is skipped until its cached
`resets_at` (or 30 minutes).

A turn that fails is classified and retried: a logged-out account is marked and
you are told how to fix it, a rate-limited one goes into cooldown, a network
blip is retried on the same account, and the turn moves to another healthy
account with the **same session id** — which works because the accounts share
`projects/`.

### Environment scrubbing

Every `claude` ccc spawns gets an explicitly built environment (plus whatever
`env_passthrough` names) — `PATH`, `HOME`,
`USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TMPDIR`, `TZ`, `TERM`, `XDG_*`,
`SSH_AUTH_SOCK`, plus the profile's `CLAUDE_CONFIG_DIR` — and nothing else. No
parent `CLAUDE*` / `ANTHROPIC*` variable is passed through. This is not
hygiene theatre: when ccc is started from inside a Claude Code session or the
desktop app, the parent exports `CLAUDECODE=1`, `ANTHROPIC_BASE_URL`,
`CLAUDE_CODE_OAUTH_SCOPES` and friends, and a child `claude` that inherits them
authenticates as something other than the profile you asked for.

## Known behaviors

Claude Code behaviors ccc lives with rather than changes. These were verified
empirically against 2.1.270 on macOS; the probes are described in `runner.go`.

- **`--system-prompt` does not suppress `CLAUDE.md`.** A run with a custom
  system prompt still loads the project *and* user `CLAUDE.md`. Only
  `--setting-sources ''` (an empty list) loads neither; `--setting-sources user`
  still loads the owner's personal `~/.claude/CLAUDE.md`. That is why ccc passes
  the empty form — a bot must not inherit your global instructions.
- **`bypassPermissions` survives the empty setting sources.** The
  once-per-config-dir disclaimer acceptance is not read from the user settings
  file, so bots keep working with no settings loaded.
- **The system prompt is recorded per conversation.** A resumed session ignores
  a different `--system-prompt` and answers from the original one, and
  `--system-prompt-snapshot off` does *not* change that. Everything that varies
  per turn (memories, inbox, the date) therefore travels in the message
  envelope, and changing a role rotates the session.
- **Flags are not restored on resume.** `--mcp-config`, `--settings` and friends
  must be passed on every single turn.
- **`--bare` is not an option.** It disables OAuth entirely (API key only).
- **`claude auth status --json` exits 1 when logged out** while still printing
  valid JSON, so the payload is authoritative and the exit code is not.
- **`-p` turns leave nothing behind.** They never appear in `claude agents`.

## Configuration

Two places hold state:

- `~/.config/ccc/config.json` — **bootstrap** config: bot token, group, Claude
  accounts, model, data dir.
- `<data_dir>/ccc.db` — **runtime** state in SQLite (WAL): bots, turns, memories,
  inbox, questions. `data_dir` defaults to `~/.local/share/ccc`; bot workspaces
  live in `<data_dir>/bots/<name>/workspace`.

```json
{
  "bot_token": "…",
  "chat_id": 123456789,
  "group_id": -1001234567890,
  "data_dir": "/home/user/.local/share/ccc",
  "model": "sonnet",
  "env_passthrough": ["GH_TOKEN", "LINEAR_API_KEY"],
  "profiles": {
    "default": { "config_dir": "",                         "label": "claude's default config dir" },
    "work2":   { "config_dir": "/home/user/.claude-work2", "label": "me@example.com" }
  },
  "default_profile": "default",
  "transcription_lang": "es"
}
```

`env_passthrough` is the only channel by which a secret reaches a bot: those
variable names are copied from ccc's environment into each turn, on top of the
scrubbed whitelist. `CLAUDE*` / `ANTHROPIC*` names are never passed through.

v3 does not migrate v2's `sessions` map or the JSONL ledger; it is a fresh
start. The old keys are ignored and can be deleted.

## Permissions

Bots run with `--permission-mode bypassPermissions` on your machine, so **the
chat is the trust boundary**. Only the configured owner may talk to the bots
(a pairing/allowlist system for other users is coming). A bot loads no
`CLAUDE.md`, no user/project/local settings and no skills, and only ccc's own
MCP server; `send_file` refuses anything inside a credentials or config
directory.

## Notes

- Turns are `claude -p` processes: they never appear in `claude agents`, and
  `/stop` signals the whole process group so a long `go test` dies with them.
- A bot's conversation is a session UUID ccc mints and resumes. All accounts of
  one instance share a `projects/` directory, so a retried turn can resume the
  same conversation on a different account.
- Live end-to-end validation against the real `claude` and the real MCP server
  lives in `live_runner_test.go`: `go test -tags live -run TestLive -v`.
- The design this implements is `docs/DESIGN.md`.

## License

[MIT License](LICENSE)
