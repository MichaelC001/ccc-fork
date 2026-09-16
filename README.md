# ccc

**ccc** — coding sessions in Telegram. You talk to **General** in the bot's
1:1 DM; it spawns backend workers. Sessions have no Telegram topic.

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Phone client (MIT, public): [ccc-app](https://github.com/kidandcat/ccc-app) — `ccc pair`, paste the URI in the app.

---

## What ccc is

The bot's **1:1 DM is General**, the dispatcher: you talk to it, it sees
live sessions, and it can start a backend worker (`spawn_session`) or message
one (`tell_session`). Sessions live in the backend — no Telegram topic. It
has a 30s cap — longer work must go to a session. Idle sessions waiting on
you get a short reminder in General every 10 minutes. `/session <prompt>`
still starts a worker without going through General. Sessions report only
to General (`report_to_general`). There is no role, no `/role`, no «what
should I be?» interview.

Under the hood ccc drives a coding CLI as a **stateless runner**. The default
engine is Claude Code: every message is one `claude -p` process with a
conversation id ccc mints and resumes. A session can also run on **Grok
Build** (`grok`) or **Antigravity** (`agy`) — same session, same envelope, that
CLI's native print/resume flags. ccc owns everything the runner does not —
session lifecycle, memory, scheduling, account management, access control and
the Telegram UX.

```
┌────────────┐   message    ┌──────────┐   claude -p --resume  ┌──────────────┐
│  Telegram  │─────────────▶│   ccc    │──────────────────────▶│   one turn   │
│  DM = Gen. │◀─────────────│  listen  │◀──  stream-json   ────│              │
└────────────┘   progress   └──────────┘                       └───────┬──────┘
                                  ▲                                    │
                                  │        ccc mcp (stdio)             │
                                  └────────────────────────────────────┘
              remember · recall · ask_owner · watch · schedule_wakeup ·
              run_background · spawn_session · tell_session · report_to_general ·
              set_name · get_project · send_file
```

### Concepts

| | |
|---|---|
| **Instance** | One `ccc listen` process on one machine, bound to one Telegram bot token. The owner's DM is General. |
| **Session** | A backend worker. A name, a working directory, an engine and a conversation. The owner never writes into a session chat; reports come to General. |
| **Turn** | One engine process (`claude -p`, `grok --single`, or `agy --print`): one input, one answer. One turn per session at a time; messages that arrive meanwhile are folded into the next turn. |
| **Engine** | Which CLI an **account** runs: `claude`, `grok` / `grok-build`, or `antigravity` / `agy`. Set when you add the account (`/account add <identity> <engine>`). A session's turns pick a healthy account from that engine's pool. `/engine` is a secondary way to assign a session onto another pool. |
| **Account** | One login for one engine. Claude = `CLAUDE_CONFIG_DIR`. Grok = isolated `GROK_HOME`. Antigravity = isolated `HOME` (`~/.gemini`). One ccc process can hold several Claude emails + several Grok logins + several agy logins at once. Failover stays inside the same engine. |
| **Memory** | Durable facts in `user` (about you, shared across sessions) and `project` (about one code base). A leftover per-session scope still exists internally; it is not a persona. |
| **Watch** | A command re-run on an interval. The session is woken **only when the output changes**, with a diff. Nothing changing costs nothing. Lives 4 hours, then it is cancelled and the session is woken to re-set it. Standing jobs are routines. |
| **Schedule** | A wakeup at a time, or on a cron expression. |
| **Background job** | A long shell command on a session. The session stays responsive; it is woken when the job finishes. |

### Phone app

Anyone can run `ccc listen`, install the [CCC](https://github.com/kidandcat/ccc-app) app, and pair:

```
ccc pair          # on the machine
# paste the ccc://pair/v1?… URI in the app
```

The default hub (`wss://hub.mentasystems.com`) is a free encrypted relay. It cannot read chats. `ccc hub` runs your own.

### What a session sees

- A system prompt with its name, machine and working directory.
- Per message, a small `<context>` envelope: today's date and the most relevant
  memories.
- **No `CLAUDE.md`, no user/project settings, no skills.** Sessions run with
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

### 3. Create the Telegram bot

On your phone:

1. Talk to [@BotFather](https://t.me/BotFather) → `/newbot` → copy the token.
2. Get your own numeric Telegram user id from [@userinfobot](https://t.me/userinfobot).
3. DM the bot — that chat **is General**.

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

**`env_passthrough` and secrets.** Those names are the only channel by which a
secret reaches a bot. The service is started by `systemctl --user`, which never
sources `~/.profile` or `~/.zshrc`, so ccc snapshots the VALUES into
`~/.config/ccc/env` (mode 0600) and the unit reads that file — the unit itself
holds no secrets. `ccc config set env_passthrough …` writes it, and so does
`ccc install`. **Run them from a login shell**, or the values will not be
visible:

```bash
bash -lc 'ccc env sync'      # re-snapshot after exporting a new secret
```

Each of those commands prints which names it found and which are missing — names
only, never values — and `/status` shows the same thing from Telegram. Change a
token? Export it and run `ccc env sync` again, then restart the service.

### 5. Install the service

```bash
bash -lc 'ccc install'            # login shell: it snapshots env_passthrough into ~/.config/ccc/env
                                  # writes ~/.config/systemd/user/ccc.service and starts it
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

### 6. Log an account in — from your phone

DM the bot (or anywhere):

```
/account add you@example.com claude
```

(`/account add you@example.com` still works: omitted engine is Claude.)

An account is its identity plus its engine. The same email can be registered
on more than one engine (a Claude profile and a Codex profile may share
`you@example.com`). Claude identities are emails; Grok and Antigravity
accept a short name or email. ccc derives an isolated home
behind it (you never see or type that path). For Claude it runs
`claude auth login` on a pseudo-terminal and posts the login URL. Open it on a
device where you are signed in to the right account, and send the code it gives
you back into the same chat. ccc feeds it to the CLI, confirms with
`claude auth status`, then accepts the bypass-permissions disclaimer for you.

Grok and Antigravity use the same `/account` flow with their own CLI login
(`grok login --device-auth`, `agy auth login`) and isolated homes.

If the device turned out to be signed in as somebody else, ccc says so and
stores the account under the address it actually logged in as. The account
already on the machine (`~/.claude`) needs no `add`: it shows up under its own
email as soon as ccc has asked `claude auth status` once.

> ccc **never opens a browser** on the machine it runs on: the login URL is for
> Telegram only. The login runs with a directory of no-op `open` / `xdg-open`
> shims first on `PATH` and `$BROWSER` pointed at one of them.

Add more accounts the same way. Mix engines in one instance:

```
/account add other@example.com claude
/account add work grok
/account add lab agy
```

Turns pick a healthy account whose engine matches the session. Failover stays
Claude↔Claude or Grok↔Grok — ccc does not jump Claude→Grok mid-conversation.

### 7. Start your first session

DM the bot:

```
keep an eye on the fecha deploy and tell me if anything breaks
```

That chat is General, the dispatcher: it will `spawn_session` (or you can
`/session` the prompt yourself). Sessions run in the backend and report
back here. There is no worker topic to talk in.

---

## Using it

### Conversation

| Where | What happens |
|---|---|
| Text in the **DM** (General) | A turn of the dispatcher (30s cap). It sees live sessions and can spawn or tell them. Idle workers waiting on you get a reminder here every 10 minutes. |
| `/session <prompt>` | Starts a backend worker named after the first line, first turn = that prompt. |
| A photo or document | Saved into General's `inbox/`, with the path passed in the message. |
| A voice note | Transcribed if the `voice` build is installed, else the file path is passed. |
| A reply to a question / a button | Answers that session's `ask_owner`. Free text in the DM is always General. |

While a General turn runs, one progress message in the DM is edited in place
(no Telegram notification). The answer is posted when the turn finishes — that
is the ping you get — and your message gets a ✅. Workers report back through
General (`report_to_general`).

### Commands

**In the DM (General)**

| Command | Effect |
|---|---|
| `/name [name]` | Show or set the session's name (General stays General). Starts a fresh conversation (the name is in the system prompt). Names are unique. |
| `/new` | Fresh conversation. Memories are kept. |
| `/stop` | Kill the running turn and drop the queue. |
| `/cwd [path]` | Show or set the session's working directory. |
| `/engine [name]` | Show this session's engine pool, or assign it to another (`claude`, `grok`, `antigravity`). Secondary: engine is set when you add the account. Switching pools starts a fresh conversation. |
| `/memory [query]` | List or search the memories this session can see. |
| `/memory stats` | Per scope: how many entries, how many bytes, whether it is due for compaction, and when it was last compacted. |
| `/memory restore <id>` | Undo one memory compaction (owner only). The id is in the compaction message and in `/memory stats`. |
| `/forget <scope> <key>` | Delete one memory. |
| `/watches` / `/watches cancel <name>` | List or cancel its watches. |
| `/schedules` / `/schedules cancel <id>` | List or cancel its wakeups. |

**Anywhere**

| Command | Effect |
|---|---|
| `/sessions` | Every open session, its status and when it last ran. (`/bots` still works.) |
| `/status` | Queue, running turns, accounts, watches, schedules, passthrough secrets (names only) and doctor findings. |
| `/usage` | Tokens in/out, cache hit ratio, turns, average duration and cost — per session and in total, for today and the last 7 days. |

**Owner only**

| Command | Effect |
|---|---|
| `/account` | Status card per account (engine + health), with buttons. |
| `/account add <identity> <engine>` | Register an account for that engine and start its login. |
| `/account login\|remove\|default <identity> [engine]` | Relogin, remove, or make default (new sessions inherit that account's engine). If the same email exists on several engines, pass `email/codex` or `email codex`. |
| `/access` | Who may talk to ccc (see below). |
| `/model [name]` | Show or set the model every session runs on. `/model default` clears it. |

### What a session can do for itself

Every Claude, Grok and Codex session has these tools (Antigravity does not;
use `ccc routine` there). Grok calls them through `search_tool` / `use_tool`.

- `remember` / `recall` / `forget` — durable memory in `user` and `project`
  (plus a leftover per-session scope).
- `notify_owner` / `ask_owner` — reach you; `ask_owner` renders inline buttons
  and the turn ends until you answer.
- `watch` / `unwatch` / `list_watches` — a command re-run on an interval that
  wakes the session only when its output changes. Lasts `watch_ttl_s` (default 4 h),
  then it is cancelled and the session is woken to re-set it. Standing jobs are
  `set_routine`.
- `schedule_wakeup` / `cancel_schedule` — one-off (or unnamed cron) wakeups.
- `set_routine` / `list_routines` / `cancel_routine` — named recurring work,
  timezone-aware (default `Europe/Madrid`), ⏰ in General when it fires.
  Agy: `ccc routine add <name> --cron "0 9 * * 1-5" <prompt>`.
- `run_background` / `list_background` / `get_background` / `cancel_background`
  — start a long shell command without blocking the turn (builds, installs,
  waits). The session is woken with the result when it finishes.
- **General only:** `list_sessions`, `spawn_session`, `tell_session`.
- **Workers only:** `report_to_general` — the only way a session talks back.
- `archive_bot` — end this session.
- `get_project` / `set_project` — shared notes about a code base.
- `set_name` — rename this session.
- `send_file` — send a file to the owner (refuses credential paths).

### What it costs, and what keeps it small

**One turn per burst, not per message.** When you send three lines in a row, an
idle session waits `debounce_ms` (default 2500) for you to stop typing and answers
all of them in ONE `claude -p` run. Messages that arrive while a turn is running
already queue and are delivered together on the next one. A watch, a schedule
or a background job is never delayed. `ccc config set debounce_ms 0`
turns the wait off.

**Resumed turns are mostly cache reads.** The system prompt of a session is
byte-stable from turn to turn (the icon list is sorted, nothing that
changes per turn is in it), so the API's prompt cache covers the conversation
and only the new message is charged as fresh input. `/usage` reports the cache
hit ratio per session — if it drops, something started varying the prompt.

**Idle conversations are rotated.** After `idle_compact_s` (default 1 h) without a
turn, ccc does the same as `/new`: memories stay, the conversation does not.
Claude's prompt cache expires on that same horizon; resuming a cold fat session
would re-charge the whole history.

### Maintenance (it cleans up after itself)

Once a day at `maintenance_hour` (default 04:00 local) — or on demand with
`ccc maintain` — ccc keeps its own database small:

- **Turns**: it keeps 30 days OR the last 200 per session, whichever keeps more, and
  after a week it replaces a turn's text with the first 500 characters. The
  status and the token usage are kept, so `/usage` still works on old turns.
- **Memories**: when one scope (your `user` memories, a project's, a session's)
  passes 120 entries or 48 KB, ONE turn on a cheap model (`compaction_model`,
  default `haiku`) merges the duplicates and drops what a newer entry
  contradicts. It runs outside every session, with no tools and no access to
  anything. You get a message in **General**:

  ```
  🧹 Compacted user memories: 143 → 61 (/memory restore 4 to undo)
  ```

  The originals are archived, so `/memory restore 4` puts them back exactly.
  If the result does not parse, or if it dropped more than 60% of the entries,
  **nothing is applied** and you are told why instead.
- **Cleanup**: delivered leftover inbox rows and answered questions older than
  30 days, the private notes of sessions archived over a month ago, and memory
  archives older than 90 days.

Tuning knobs. These live in `config.json` and have no Telegram command: the
defaults are meant to be right, and `ccc config` prints what is in force.

| Setting | Default | What it does |
|---|---|---|
| `debounce_ms` | 2500 | How long an idle bot waits for more messages before starting a turn. 0 disables it. |
| `compaction_model` | `haiku` | The cheap model the memory compaction runs on. An unknown name falls back to the instance model. |
| `maintenance_hour` | 4 | Local hour the daily job runs at. A machine that was off catches up when it wakes. |
| `idle_compact_s` | 3600 | Seconds a conversation may sit unused before it is rotated (`/new`). 0 disables. |
| `watch_ttl_s` | 14400 | Seconds a watch lives before it is cancelled and the session is woken. 0 disables. Routines do not expire. |

```bash
ccc config                              # every key, with the defaults in force
ccc config set debounce_ms 0
ccc config set compaction_model sonnet
ccc config set maintenance_hour 22
ccc config set idle_compact_s 3600
ccc config set watch_ttl_s 14400
```

### Access control

ccc is **default-deny by Telegram user id**. The owner (`chat_id`) is always
allowed; nobody else can do anything until you approve them.

- A stranger's message **in a group** is dropped in silence.
- A stranger's **DM** gets one reply with a 6-hex pairing code (valid an hour;
  at most 3 pending requests, at most 2 replies per person and then silence).
  You also get a DM with **Allow** / **Block** buttons.
- You approve with the button or `/access pair <code>`.
- `/access list`, `/access add <id>`, `/access remove <id>`, `/access block <id>`.

An approved user can talk in the DM (General). They cannot use `/account`, `/access`,
`/model` — those stay yours.

> Sessions run with bypassed permissions on your machine. **The chat is the trust
> boundary**, which is why this is not optional. Secrets reach sessions only through
> `env_passthrough`; ccc never posts environment values or credential files, and
> `send_file` refuses anything inside a config or credentials directory.

---

## Engines and accounts

Engine is a property of an **account**, set when you add it. One ccc process
can mix several Claude emails, several Grok logins, several Antigravity
logins and several Codex logins — including the **same email on different
engines**. A session's turns pick a healthy account from
the pool that matches what that session runs on. New sessions inherit the default
account's engine (or `default_engine` if you set one). `/engine` only assigns
a session onto another already-registered pool — it is not the way you introduce
an engine.

The **model** is not a property of the account. `/model <slug>` in a session
topic overrides that session; `/model <engine> <slug>` in General (or a DM) sets
the instance default for that engine. Empty means the CLI's own default.

| Engine | Add account | Isolated home | Binary | Session | MCP |
|---|---|---|---|---|---|
| **Claude Code** | `/account add you@x.com claude` | `CLAUDE_CONFIG_DIR` under `<data_dir>/profiles/` | `claude` | ccc mints a UUID; `--session-id` then `--resume` | ccc MCP (`remember`, `ask_owner`, `run_background`, …) |
| **Grok Build** | `/account add work grok` | `GROK_HOME` = `<data_dir>/accounts/grok/<id>` (`auth.json`) | `grok` (`~/.grok/bin/grok`) | ccc mints a UUID; `--session-id` then `--resume` | `[mcp_servers.ccc]` in isolated GROK_HOME |
| **Antigravity** | `/account add lab agy` | isolated `HOME` + `GEMINI_HOME` + `GEMINI_FORCE_FILE_STORAGE` under `<data_dir>/accounts/antigravity/<id>` | `agy` (`~/.local/bin/agy`) | first turn lets `agy` mint a `conversation_id`; later turns pass `--conversation` | not wired |
| **Codex** | `/account add openai codex` | `CODEX_HOME` = `<data_dir>/accounts/codex/<id>` (`auth.json`) | `codex` (PATH / `~/.local/bin/codex`) | first turn lets Codex mint a `thread_id`; later turns `codex exec resume <id>` | CODEX_HOME config.toml + per-turn `-c` |

```
/account add you@example.com claude
/account add you@example.com codex   # same email, different engine
/account add work grok
/account add lab antigravity
/account add openai codex
/account                         # mixed list: identity, engine, health
/account login you@example.com/codex # required when that email is on several engines

/engine                          # which pool this session uses
/engine grok                     # assign this session to the Grok pool (secondary)
/model grok grok-4               # instance default for that engine
/model gpt-5.4                   # this session (in its topic)

ccc config set default_engine grok    # optional override for new sessions
ccc config get default_engine
```

**Claude** is unchanged: profile pool, `CLAUDE_CONFIG_DIR`, same-engine
failover, MCP, `--output-format stream-json`.

**Grok Build.** `/account add work grok` creates an isolated `GROK_HOME` and
drives `grok login --device-auth` on a PTY (the VM has no browser). Turns set
`GROK_HOME` so `auth.json` is this account, not `~/.grok`. Failover is
Grok↔Grok. Turns run `grok --always-approve --output-format
streaming-messages-json --single <envelope>`. Guess labeled: the `grok`
binary was not on the machine that built this; flags were checked against
Grok Build's published headless docs and Hairok's Mac-verified set.

**Antigravity.** `/account add lab agy` creates an isolated HOME so
`~/.gemini` cannot collide with Claude profiles or the real user home, and
forces file-store tokens (`GEMINI_FORCE_FILE_STORAGE=true`) instead of the OS
keyring. Login is `agy auth login` as the CLI requires. A headless run that
is not already logged in exits with `authentication required`. Turns run
`agy --print <prompt> --output-format stream-json --dangerously-skip-permissions`
and `--conversation` on resume. `--print-timeout 120m` is a ccc choice (agy's
default is 5 minutes). Guess labeled: `agy` was not on the build machine;
flags match the official headless docs. Isolation is HOME/XDG/`GEMINI_HOME`
because agy has no official profile selector.

Progress streaming works when the CLI emits a usable NDJSON stream. If an
engine only prints the final answer, Telegram still gets that text (the
progress line stays at "thinking").

`/model` is still instance-wide and is passed through as `--model` on every
engine when set. Use a slug that engine accepts, or `/model default` for the
CLI's own default. An unknown agy model fails the turn.

`ccc doctor` reports `grok` and `agy` as optional.

---

## Where things live

| What | Where |
|---|---|
| Bootstrap config (token, owner, group, accounts) | `~/.config/ccc/config.json` (mode 0600) |
| Passthrough secrets for the service | `~/.config/ccc/env` (mode 0600, written by `ccc env sync`) |
| Everything runtime (bots, turns, memories, watches, schedules, access) | `<data_dir>/ccc.db` (SQLite, WAL) |
| A bot's default working directory | `<data_dir>/bots/<name>/workspace` |
| Files you send a bot | `<its cwd>/inbox/` |
| Claude accounts | `<data_dir>/profiles/<name>` (one `CLAUDE_CONFIG_DIR` each) |
| Grok / Antigravity accounts | `<data_dir>/accounts/grok/<id>` (`GROK_HOME`) and `<data_dir>/accounts/antigravity/<id>` (isolated HOME) |
| Shared conversation transcripts | `<data_dir>/projects`, symlinked into every Claude account |
| Logs | `~/Library/Caches/ccc/ccc.log` (macOS) or `journalctl --user -u ccc` |

`data_dir` defaults to `~/.local/share/ccc`.

Claude accounts point their `projects/` at the **same** directory on purpose:
that is what lets a turn refused by one Claude account resume the very same
conversation on another. Grok and Antigravity keep isolated homes and fail
over only inside their own pool.

---

## Fixing a command you mistyped

Edit the message. An edited message whose text starts with `/` runs as a
command, once per edit — so turning `/account add` into
`/account add you@example.com` does what you meant without sending a second
message. Editing plain text still does nothing: correcting a typo must not
re-run a turn.

---

## Troubleshooting

**A bot says an account needs a new login.** You will also get a DM with a
**Relogin** button. Tap it, or send `/account login <email>`, and follow the URL
+ code flow. The account is skipped for selection until it is fixed.

**`organization has disabled Claude subscription access`.** This reads like an
administrator blocked you, but in practice it is a stale OAuth token. It is
classified as `auth_stale`: the turn is retried on another account and the
profile is marked. Fix it with `/account login <email>`.

**Every account is rate limited.** The turn reports it and the accounts go on
cooldown until their cached reset time. `/status` shows the cooldowns.

**A bot cannot see `GH_TOKEN` (or any other secret).** `/status` lists the
`env_passthrough` names it can and cannot see. A missing one almost always means
`ccc install` / `ccc env sync` was run from a non-login shell, so the value was
never snapshotted into `~/.config/ccc/env`. Fix it with
`bash -lc 'ccc env sync'` and restart the service. The file is 0600 and the
systemd unit contains no secrets, only `EnvironmentFile=-%h/.config/ccc/env`.

**A compaction dropped something I wanted.** `/memory stats` shows the last
compaction id per scope; `/memory restore <id>` puts the originals back and
undoes it. Raise the thresholds by keeping fewer memories, or set
`ccc config set compaction_model` to a stronger model if the cheap one
consolidates badly.

**`systemctl --user` fails with "Failed to connect to bus".** Export
`XDG_RUNTIME_DIR=/run/user/$(id -u)` and make sure `loginctl enable-linger
$USER` is on. See step 5.

**Nothing responds in the DM.** Check `/status`. The usual causes are a
`chat_id` that is not your user id, or the bot token.

**A message got no reply and no error.** You are probably not the owner and not
approved — access control drops group messages from unknown users silently. Ask
the owner for `/access add <your id>`.

**A watch never fires.** Its first run is a baseline, not a change. Check it
with `/watches`; the interval floor is 60 s.

**Run the diagnostics.** `ccc doctor` checks the claude binary, every account's
login and disclaimer state, the configuration and the service. `ccc doctor
--fix` also records the bypass-permissions disclaimer for any account missing
it (the same write as `ccc profile accept-disclaimer <email>`).

---

## CLI

```
ccc listen                    Run the instance (the service does this)
ccc setup <bot_token>         Interactive bootstrap (owner DM, service)
ccc config [get|set] …        Non-interactive bootstrap
ccc install                   Install the service (launchd / systemd --user)
ccc env sync                  Snapshot env_passthrough secrets into ~/.config/ccc/env
                              (run from a login shell: bash -lc 'ccc env sync')
ccc maintain                  Run the daily growth-control job once, now
ccc doctor [--fix]            Check dependencies and configuration; --fix records
                              the bypass disclaimer for accounts missing it
ccc profile <cmd>             Manage accounts from a shell
                              (list/add/remove/default/login/accept-disclaimer)
ccc send <file>               Send a file to the owner from the session owning this directory
ccc relay [port]              Relay server for files over 50 MB
ccc pair                      Print a URI to add this machine to the CCC phone app
ccc unpair                    List or revoke paired devices
ccc hub [addr]                Run the public pairing hub (default :8787)
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
