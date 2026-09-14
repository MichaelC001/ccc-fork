# ccc

> A team of Claude bots living in one Telegram forum group. Each topic is a bot
> with its own role, memory and workspace; you talk to it like you talk to a
> person.

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## What ccc is

A **topic is a bot**, not a session. Each bot has a name, a free-text role you
set with `/role`, its own working directory, its own Claude conversation and a
memory shared with the rest of the team. You send it a message; it does the work
and answers in the topic. There are no commands in the normal flow.

Under the hood ccc drives Claude Code as a **stateless runner**: every message is
one `claude -p` process with a session id ccc mints and resumes. ccc owns
everything the runner does not — bot identity, memory, inter-bot messaging,
scheduling, account management, access control and the Telegram UX.

```
┌────────────┐   message    ┌──────────┐   claude -p --resume  ┌──────────────┐
│  Telegram  │─────────────▶│   ccc    │──────────────────────▶│   one turn   │
│   topic    │◀─────────────│  listen  │◀──  stream-json   ────│              │
└────────────┘   progress   └──────────┘                       └───────┬──────┘
                                  ▲                                    │
                                  │        ccc mcp (stdio)             │
                                  └────────────────────────────────────┘
              remember · recall · ask_owner · send_to_bot · watch ·
              schedule_wakeup · spawn_bot · get_project · send_file
```

### Concepts

| | |
|---|---|
| **Instance** | One `ccc listen` process on one machine, bound to one Telegram bot token and one forum group. |
| **Bot** | One forum topic. A name, a role, a working directory and a memory scope. |
| **Turn** | One `claude -p` run: one input, one answer. One turn per bot at a time; messages that arrive meanwhile are folded into the next turn. |
| **Account** | One Claude login = one `CLAUDE_CONFIG_DIR`. Each turn picks the healthiest account; a turn refused by one is retried on another. |
| **Memory** | Durable facts in three scopes: `user` (about you, shared by all bots), `project` (about one code base) and `bot` (private). |
| **Watch** | A command re-run on an interval. The bot is woken **only when the output changes**, with a diff. Nothing changing costs nothing. |
| **Schedule** | A wakeup at a time, or on a cron expression. |

### What a bot sees

- A system prompt with its name, role, machine, working directory and the other
  bots on the team.
- Per message, a small `<context>` envelope: today's date, the most relevant
  memories and a summary of anything waiting in its inbox.
- **No `CLAUDE.md`, no user/project settings, no skills.** Bots run with
  `--setting-sources ''`, so nothing you have installed for yourself leaks into
  them. They run with bypassed permissions in their own working directory.

---

## Bootstrap on a VM, step by step

This is the headless path: no terminal is ever attached to Telegram, and no
browser is ever needed on the server.

### 1. Build and copy the binary

On your machine:

```bash
git clone https://github.com/kidandcat/ccc && cd ccc
make build-linux                       # pure Go, CGO_ENABLED=0, no libc dependency
scp ccc-linux-amd64 vps:~/bin/ccc
```

### 2. Install Claude Code on the VM

```bash
ssh vps
curl -fsSL https://claude.com/install.sh | bash    # or your usual install method
claude --version
```

### 3. Create the Telegram bot and the group

On your phone:

1. Talk to [@BotFather](https://t.me/BotFather) → `/newbot` → copy the token.
2. Create a **group**, open its settings and enable **Topics**.
3. Add your bot to the group and make it an **admin** (it must be able to
   create and close topics).
4. Get your own numeric Telegram user id from [@userinfobot](https://t.me/userinfobot).

### 4. Configure ccc (no interaction needed)

```bash
ccc config set bot_token 123456:ABC-YourTokenHere
ccc config set chat_id   11111111          # YOUR telegram user id — this is the owner
ccc config set data_dir  ~/.local/share/ccc
ccc config set model     sonnet            # optional; omit for claude's default
ccc config set env_passthrough GH_TOKEN,LINEAR_API_KEY   # optional secrets for bots
ccc config                                  # check it
```

`chat_id` is the access-control root: only that user id can administer the
instance, and until it is set **nobody** can talk to ccc at all.

### 5. Install the service

```bash
ccc install                       # writes ~/.config/systemd/user/ccc.service and starts it
loginctl enable-linger $USER      # so it keeps running after you log out
systemctl --user status ccc
journalctl --user -u ccc -f
```

> **`systemctl --user` over ssh:** a plain non-login ssh session often has no
> `XDG_RUNTIME_DIR`, and every `systemctl --user` call then fails with
> *"Failed to connect to bus"*. Fix it per command or in your shell profile:
>
> ```bash
> export XDG_RUNTIME_DIR=/run/user/$(id -u)
> ```
>
> With `enable-linger` on, that directory exists even when you are not logged
> in. If it does not, `loginctl enable-linger $USER` and log in again.

### 6. Bind the group and log an account in — from your phone

In the forum group, send:

```
/setgroup
```

ccc records the group id. Then, anywhere:

```
/account add work
```

ccc creates a config dir for the account, runs `claude auth login` on a
pseudo-terminal and posts you the login URL. Open it on a device where you are
signed in to the right Claude account, and send the code it gives you back into
the same chat. ccc feeds it to the CLI, confirms with `claude auth status`, then
accepts the bypass-permissions disclaimer for you.

> ccc **never opens a browser** on the machine it runs on: the login URL is for
> Telegram only. The login runs with a directory of no-op `open` / `xdg-open`
> shims first on `PATH` and `$BROWSER` pointed at one of them.

Add a second account the same way (`/account add personal`) — turns are spread
across them and fail over when one runs out.

### 7. Make your first bot

Send a message in the group's **General** topic:

```
keep an eye on the fecha deploy and tell me if anything breaks
```

ccc creates a topic named after the first line, with a bot behind it, and
dispatches your message. From then on, talk in that topic.

---

## Using it

### Conversation

| Where | What happens |
|---|---|
| Text in **General** | Creates a new bot named after the first line, and sends it your message. |
| Text in a **bot's topic** | An input for that bot. |
| A photo or document | Saved into the bot's `inbox/`, with the path passed in the message. |
| A voice note | Transcribed if the `voice` build is installed, else the file path is passed. |
| A reply to a question | Answers it. Any text while a bot is waiting counts as the answer too. |

While a turn runs, one progress message in the topic is edited in place with
what the bot is doing. It is replaced by the answer, and your message gets a ✅.

### Commands

**In a bot's topic**

| Command | Effect |
|---|---|
| `/role [text]` | Show or set the bot's role. Setting it starts a fresh conversation. |
| `/new` | Fresh conversation. Memories are kept. |
| `/stop` | Kill the running turn and drop the queue. |
| `/cwd [path]` | Show or set the bot's working directory. |
| `/memory [query]` | List or search the memories this bot can see. |
| `/forget <scope> <key>` | Delete one memory. |
| `/watches` / `/watches cancel <name>` | List or cancel its watches. |
| `/schedules` / `/schedules cancel <id>` | List or cancel its wakeups. |

**Anywhere**

| Command | Effect |
|---|---|
| `/bots` | Every bot, its status and when it last ran. |
| `/status` | Queue, running turns, accounts, watches, schedules and doctor findings. |

**Owner only**

| Command | Effect |
|---|---|
| `/account` | Status card per Claude account, with buttons. |
| `/account add\|login\|remove\|default <name>` | Manage accounts (see above). |
| `/access` | Who may talk to ccc (see below). |
| `/model [name]` | Show or set the model every bot runs on. `/model default` clears it. |
| `/setgroup` | Bind ccc to the forum group the command was sent in. |

### What a bot can do for itself

Every bot has these tools, and uses them without being told:

- `remember` / `recall` / `forget` — durable memory in the three scopes.
- `list_bots` / `send_to_bot` — message a teammate; the exchange is mirrored
  into both topics as 🤝, and the recipient wakes up with it.
- `notify_owner` / `ask_owner` — reach you; `ask_owner` renders inline buttons
  and the bot's turn ends until you answer.
- `watch` / `unwatch` / `list_watches` — a command re-run on an interval that
  wakes the bot only when its output changes.
- `schedule_wakeup` / `cancel_schedule` — one-off or cron wakeups.
- `spawn_bot` / `archive_bot` — create a helper with its own topic, which
  reports back with `send_to_bot`; archive it when the job is done.
- `get_project` / `set_project` — the team's shared notes about a code base.
- `update_instructions` — rewrite its own role.
- `send_file` — send a file into its topic (refuses credential paths).

### Access control

ccc is **default-deny by Telegram user id**. The owner (`chat_id`) is always
allowed; nobody else can do anything until you approve them.

- A stranger's message **in the group** is dropped in silence.
- A stranger's **DM** gets one reply with a 6-hex pairing code (valid an hour;
  at most 3 pending requests, at most 2 replies per person and then silence).
  You also get a DM with **Allow** / **Block** buttons.
- You approve with the button or `/access pair <code>`.
- `/access list`, `/access add <id>`, `/access remove <id>`, `/access block <id>`.

An approved user can talk to the bots. They cannot use `/account`, `/access`,
`/model` or `/setgroup` — those stay yours.

> Bots run with bypassed permissions on your machine. **The chat is the trust
> boundary**, which is why this is not optional. Secrets reach bots only through
> `env_passthrough`; ccc never posts environment values or credential files, and
> `send_file` refuses anything inside a config or credentials directory.

---

## Where things live

| What | Where |
|---|---|
| Bootstrap config (token, owner, group, accounts) | `~/.config/ccc/config.json` (mode 0600) |
| Everything runtime (bots, turns, memories, watches, schedules, access) | `<data_dir>/ccc.db` (SQLite, WAL) |
| A bot's default working directory | `<data_dir>/bots/<name>/workspace` |
| Files you send a bot | `<its cwd>/inbox/` |
| Claude accounts | `<data_dir>/profiles/<name>` (one `CLAUDE_CONFIG_DIR` each) |
| Shared conversation transcripts | `<data_dir>/projects`, symlinked into every account |
| Logs | `~/Library/Caches/ccc/ccc.log` (macOS) or `journalctl --user -u ccc` |

`data_dir` defaults to `~/.local/share/ccc`.

All accounts point their `projects/` at the **same** directory on purpose: that
is what lets a turn refused by one account resume the very same conversation on
another.

---

## Troubleshooting

**A bot says an account needs a new login.** You will also get a DM with a
**Relogin** button. Tap it, or send `/account login <name>`, and follow the URL
+ code flow. The account is skipped for selection until it is fixed.

**`organization has disabled Claude subscription access`.** This reads like an
administrator blocked you, but in practice it is a stale OAuth token. It is
classified as `auth_stale`: the turn is retried on another account and the
profile is marked. Fix it with `/account login <name>`.

**Every account is rate limited.** The turn reports it and the accounts go on
cooldown until their cached reset time. `/status` shows the cooldowns.

**`systemctl --user` fails with "Failed to connect to bus".** Export
`XDG_RUNTIME_DIR=/run/user/$(id -u)` and make sure `loginctl enable-linger
$USER` is on. See step 5.

**Nothing responds in the group.** Check `/status` in the owner's DM. The usual
causes are a `group_id` that does not match the group (`/setgroup` fixes it),
the bot not being an admin, or Topics not enabled.

**A message got no reply and no error.** You are probably not the owner and not
approved — access control drops group messages from unknown users silently. Ask
the owner for `/access add <your id>`.

**A watch never fires.** Its first run is a baseline, not a change. Check it
with `/watches`; the interval floor is 60 s.

**Run the diagnostics.** `ccc doctor` checks the claude binary, every account's
login and disclaimer state, the configuration and the service.

---

## CLI

```
ccc listen                    Run the instance (the service does this)
ccc setup <bot_token>         Interactive bootstrap (owner, group, service)
ccc config [get|set] …        Non-interactive bootstrap
ccc setgroup                  Record the group from your next message in it
ccc install                   Install the service (launchd / systemd --user)
ccc doctor                    Check dependencies and configuration
ccc profile <cmd>             Manage accounts from a shell (list/add/remove/default/login)
ccc send <file>               Send a file into the topic of the bot owning this directory
ccc relay [port]              Relay server for files over 50 MB
ccc mcp --bot <id>            MCP server for one turn (spawned by Claude Code)
```

## Building

```bash
make build          # this machine
make build-linux    # ccc-linux-amd64 for the VPS
make test           # go build ./... && go vet ./... && go test ./...
make build-voice    # with whisper.cpp for voice transcription (needs cmake)
make install        # build + install to ~/bin/ccc
```

Tests that spend real tokens are behind a build tag:

```bash
go test -tags live -run TestLive -v -timeout 900s .
```

## Design

The specification ccc is built to is [`docs/DESIGN.md`](docs/DESIGN.md). When
the code and that document disagree, one of them is a bug.

## License

MIT — see [LICENSE](LICENSE).
