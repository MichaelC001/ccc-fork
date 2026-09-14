# ccc - Claude Code Companion

> Control [Claude Code](https://claude.ai/claude-code) **background-agent** sessions from Telegram. Start work from your phone, talk to Claude in a topic, tap buttons to answer its questions, and pick the session back up on your PC with `claude attach`.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## What ccc is (v2)

Each ccc session is a **dedicated Claude Code background agent** — the same thing you see in `claude agents` (the fleet view). One Telegram topic maps to one bg agent with its own clean, isolated conversation. ccc mirrors that agent into Telegram: you send messages, Claude's replies come back to the topic, and when Claude needs a decision it shows the options as **inline buttons**.

This is built for **real, focused work** — one project per topic, isolated context — not fire-and-forget one-liners.

### How it works

```
┌────────────┐   message    ┌──────────┐   claude --bg     ┌──────────────┐
│  Telegram  │─────────────▶│   ccc    │──────────────────▶│  Claude bg   │
│   topic    │◀─────────────│  listen  │◀── agents --json ──│    agent     │
└────────────┘   replies    └──────────┘   + transcript     └──────────────┘
                                  ▲                                │
                                  │   PreToolUse hook (buttons)    │
                                  └────────────────────────────────┘
                                       AskUserQuestion
```

- **Dispatch** — the first message to a topic starts a bg agent (`claude --bg`) with that message as the prompt. It appears in `claude agents`.
- **Mirror (pull)** — `ccc listen` polls `claude agents --json` and the session transcript, delivering Claude's text and status (working / needs input / done) to the topic.
- **Follow-ups** — later messages resume the conversation.
- **Questions** — a scoped PreToolUse hook (installed per-agent via `--settings`, never touching your global config) turns `AskUserQuestion` into Telegram buttons and feeds your tap back to Claude.
- **Handoff** — `claude attach <id>` opens the same session in your terminal.

## Features

- **Background agents** — sessions show up in `claude agents`; no tmux
- **Dedicated context per topic** — one project, one clean conversation
- **Inline question buttons** — tap to answer Claude's `AskUserQuestion`
- **Voice & images** — voice messages are transcribed; images are handed to Claude
- **File transfer** — `ccc send <file>` (direct, or streaming relay for large files)
- **Seamless handoff** — `claude attach` to continue on your PC
- **Self-hosted** — runs entirely on your machine

## Requirements

- macOS or Linux
- Go 1.21+ (to build)
- [Claude Code](https://claude.ai/claude-code) **with background-agent support** (`claude --bg` / `claude agents`)
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
| `ccc` | Attach to this directory's bg session (or run `claude`) |
| `ccc -c` | Continue the previous local session |
| `ccc start <name> <dir> <prompt>` | Start a detached session with an initial prompt |
| `ccc send <file>` | Send a file to the current session's topic |
| `ccc doctor` | Check dependencies, profiles and configuration |
| `ccc config` | Show / set configuration |
| `ccc setgroup` | Configure the Telegram group for topics |
| `ccc profile list` | Show Claude accounts, usage and login state |
| `ccc profile add <name> <dir>` | Register a Claude account (`--label <text>`) |
| `ccc profile login <name>` | Log that account in (interactive) |
| `ccc profile default <name>` | Set the account new sessions prefer |
| `ccc profile remove <name>` | Unregister an unused account |

### Telegram (in your group)

| Command | Description |
|---------|-------------|
| `/new <prompt>` | Start a new session (topic + agent) on that prompt |
| `/new` | Reset the conversation in this topic (fresh context) |
| `/stop` | Stop this session's agent (conversation is kept) |
| `/delete` | Delete this session + topic |
| `/cleanup` | Delete all sessions + topics |
| `/list` | List sessions and their status |
| `/profiles` | List Claude accounts (profiles), usage and working agents |
| `/profile <name>` | Pin this topic's **next** dispatch to a profile |
| `/c <cmd>` | Run a shell command |
| `/stats` | System stats |
| `/update` | Update ccc from GitHub |

Send a plain message in a topic to talk to that session's Claude agent. When Claude asks a question, tap the buttons to answer. Send a voice message or image and it's forwarded to Claude. In a **private chat**, any message runs a one-shot Claude query.

## Profiles (multiple Claude accounts)

ccc can dispatch agents under **several Claude accounts at once**. A profile is
just a `CLAUDE_CONFIG_DIR`: that one variable scopes the account's credentials
(the macOS Keychain service name is derived from the directory), its
`.claude.json`, `projects/`, `jobs/`, `daemon/` and `settings.json`.

The important consequence: **the fleet is per config dir.** A background-agent
supervisor daemon runs per directory, so `claude agents --json` under profile A
lists only A's sessions and a brand-new directory lists none. ccc therefore
takes one fleet snapshot **per profile** on every poll, and a session is only
ever treated as gone when its own profile's snapshot succeeded and did not list
it — otherwise one account's outage would reap the other account's topics.

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

ccc dispatches with `--dangerously-skip-permissions`, and since Claude Code
2.1.259 `--bg` refuses that until the disclaimer has been accepted **once per
config dir**:

```
--bg with bypassPermissions requires accepting the disclaimer first.
Run `claude --dangerously-skip-permissions` once interactively.
```

So every new profile needs one interactive run before ccc can use it. There is
no way to accept it non-interactively, and ccc does not try. `ccc doctor`
reports the state per profile and prints the exact command (with the right
`CLAUDE_CONFIG_DIR`), and a dispatch blocked by the gate surfaces the same
instruction in the Telegram topic instead of failing silently.

### How a profile is chosen

New sessions go to the profile with the **lowest five-hour utilization**, ties
broken by fewer working agents and then by name. Utilization comes from
`cachedUsageUtilization` in each profile's `.claude.json` — Claude Code's own
cache, which is often stale and sometimes absent; a missing number counts as
50%. A profile that hits a usage/rate limit is skipped until its cached
`resets_at` (or 30 minutes).

Once a session has a conversation it **never changes profile**: its transcript,
job state and daemon all live inside that one config dir. `/profile <name>`
therefore only works on a topic that has not started talking yet.

### Environment scrubbing

Every `claude` ccc spawns gets an explicitly built environment — `PATH`, `HOME`,
`USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TMPDIR`, `TZ`, `TERM`, `XDG_*`,
`SSH_AUTH_SOCK`, plus the profile's `CLAUDE_CONFIG_DIR` — and nothing else. No
parent `CLAUDE*` / `ANTHROPIC*` variable is passed through. This is not
hygiene theatre: when ccc is started from inside a Claude Code session or the
desktop app, the parent exports `CLAUDECODE=1`, `ANTHROPIC_BASE_URL`,
`CLAUDE_CODE_OAUTH_SCOPES` and friends, and a child `claude` that inherits them
authenticates as something other than the profile you asked for.

## Known behaviors

These are Claude Code behaviors ccc lives with rather than changes:

- **Background agents edit inside a git worktree.** A bg session works in
  `<repo>/.claude/worktrees/` instead of your checkout, unless the repo's
  `.claude/settings.json` sets `worktree.bgIsolation: "none"`. Your working tree
  is untouched until that worktree is merged.
- **Idle sessions lose their process, not their session.** After roughly an
  hour an idle background agent's process is stopped; the entry stays in
  `claude agents --json` with its `state`. Liveness is `state`, never `pid` —
  ccc polls accordingly.
- **`claude rm` can refuse.** With uncommitted changes, unpushed commits or a
  held worktree it prints `kept <id>` and a reason instead of deleting. ccc
  treats removal as fallible.
- **Resume options are sticky.** `claude --bg --resume <uuid> <prompt>` with no
  other flags continues the session in place under the same id; passing any
  other flag makes Claude Code start a *copy* and say so on a `note:` line. ccc
  passes `--settings` on every resume (it is not restored automatically), so it
  currently takes the copy path and tracks the new ids itself.
- **Job state files are not an API.** `<config_dir>/jobs/<id>/state.json` and
  the transcript JSONL format both change between versions. ccc prefers
  `claude agents --json` and uses the files only for detail strings.

## Configuration

`~/.config/ccc/config.json`:

```json
{
  "bot_token": "…",
  "chat_id": 123456789,
  "group_id": -1001234567890,
  "sessions": {
    "myproject": { "topic_id": 42, "path": "/home/user/myproject" },
    "other":     { "topic_id": 43, "path": "/home/user/other", "profile": "work2" }
  },
  "profiles": {
    "default": { "config_dir": "",                        "label": "claude's default config dir" },
    "work2":   { "config_dir": "/home/user/.claude-work2", "label": "me@example.com" }
  },
  "default_profile": "default",
  "projects_dir": "/home/user/Projects",
  "transcription_lang": "es"
}
```

`session_id` / `short_id` are added per session at runtime (the current bg agent). `topic_id` is the stable key.

`profiles` is optional and additive. With no `profiles` block ccc synthesizes a
single implicit profile (`$CLAUDE_CONFIG_DIR` if it was set when ccc started,
else `~/.claude`) and behaves exactly as it did before multi-account support. A
session with no `profile` field runs on the default profile, so existing configs
need no migration.

## Permissions

Background agents run unattended with `--dangerously-skip-permissions` (auto-approve), so they can work without blocking. `AskUserQuestion` is the exception — it always asks you, via inline buttons.

## Notes

- `/new <prompt>` creates the topic and starts the agent in `$HOME` right away; the topic is titled after the prompt and renamed once the agent names itself.
- The bg-agent short id is the first 8 characters of the conversation UUID, and a resume that starts a copy mints both anew; ccc tracks the current pair automatically.
- Live end-to-end validation of the primitives lives in `live_test.go` (gated behind `CCC_LIVE_TEST=1`).

## License

[MIT License](LICENSE)
