# ccc v3 — design

Status: approved direction (2026-09-14). This document is the specification the
implementation follows. When code and this document disagree, fix one of them
in the same change.

## 1. What ccc v3 is

ccc is a **team of generic Claude bots living in one Telegram forum group**,
driven by `claude -p` as a stateless runner, with ccc owning everything the
runner does not: bot identity, persistent memory, inter-bot messaging,
scheduling, account (profile) management, access control, and the Telegram UX.

The experience target is Jairo's `grok-bot`: you talk to a topic like you talk
to a person. No commands in the normal flow, no ceremony, no terminal.

Non-goals (explicitly dropped from v2): Claude Code background agents, the
agents view, `claude attach` handoff, transcript scraping, the AskUserQuestion
PreToolUse hook hack. One ccc instance never talks to more than one Telegram
bot.

## 2. Runtime model

| Concept | Definition |
|---|---|
| **Instance** | One `ccc listen` process on one machine, bound to one Telegram bot token and one forum group. Instance-level config: model, env passthrough, default profile, data dir. |
| **Profile** | One Claude account = one `CLAUDE_CONFIG_DIR` (see `profiles.go`). Two profiles per instance is the normal case. Profiles are interchangeable at turn granularity (§4). |
| **Bot** | One forum topic. Identity = `name` + `role` (free text set with `/role`) + its own memory scope. All bots share the same tools, system prompt template, model and env. Optional per-bot `cwd` (default: `<data_dir>/bots/<name>/workspace`). |
| **Session** | The Claude Code conversation behind a bot: a UUID ccc mints and resumes. A bot has exactly one live session; `/new` rotates it. |
| **Turn** | One `claude -p` process: input = one user/bot/system message (plus context envelope), output = streamed events until `result`. At most one turn per bot at a time; further inputs queue (FIFO) and are delivered together on the next turn. |

## 3. Turn lifecycle

```
input (Telegram text | inbox message | schedule | watch diff)
  → enqueue(bot)                       (SQLite: turns.status=queued)
  → pick profile                       (§4)
  → build envelope                     (§9)
  → spawn: claude -p <flags>           (§3.1) with claudeEnv(profile)
  → consume stream-json                (§3.2) → Telegram progress message
  → on result: persist, post final text, run post-turn hooks (§3.3)
  → on failure: classify (§3.4) → retry on the other profile or surface
```

### 3.1 Invocation

First turn of a session:

```
claude -p --session-id <uuid> --output-format stream-json --verbose
          --permission-mode bypassPermissions          # decision: all bots bypass
          --system-prompt "<rendered template>"        # replaces Claude Code's prompt
          --mcp-config '<inline JSON pointing at `ccc mcp`>' --strict-mcp-config
          --setting-sources user                       # no project/local settings
          --model <instance model>
          [--append-system-prompt …]                   # NOT used; see §9
          "<envelope + message>"
```

Later turns: same flags with `--resume <uuid>` instead of `--session-id`.
Flags must be passed on every turn (`--settings`/`--mcp-config`/`--add-dir` are
not restored on resume — verified on 2.1.259).

Rules:
- `cwd` = the bot's workspace dir. Coding work happens via absolute paths or
  the bot `cd`-ing in Bash. The repo's `CLAUDE.md` is therefore **not**
  auto-loaded; when a bot needs a project's conventions it reads them (the
  project registry in §5 points at them). The implementer must verify how
  `--setting-sources` interacts with `CLAUDE.md` discovery under `-p` and pick
  the combination that loads no `CLAUDE.md`, no project settings, and no
  auto-memory. `--bare` is NOT an option (it disables OAuth).
- Environment: `claudeEnv(profile)` whitelist only (already implemented) plus
  the instance's `env_passthrough` list (e.g. `GH_TOKEN`, `SLACK_USER_TOKEN`,
  `LINEAR_API_KEY`). Never inherit `CLAUDE*`/`ANTHROPIC*` from the parent.
- The MCP config points at `ccc mcp --bot <id> --turn <id>` (stdio). ccc is
  therefore both the Telegram client and an MCP server binary (§6).
- Streaming: `--output-format stream-json` (+ `--include-partial-messages` if it
  proves useful for progress; otherwise per-message granularity is enough).

### 3.2 Progress in the topic

One progress message per turn, edited in place (rate-limited to ~1 edit / 3 s):
current tool activity summarized ("editing poller.go", "running go test",
"reading PR #1234"), elapsed time. When the turn ends the progress message is
replaced by the final assistant text (chunked at 4096, Telegram HTML), and a
✅ reaction is added to the user's triggering message. Tool call payloads are
never dumped into the topic; `thinking` is never shown.

### 3.3 Post-turn

- Persist `turns` row: profile used, duration, cost/usage from `result`,
  `stop_reason`, session id.
- Deliver any `send_to_bot` messages produced during the turn (they were
  written to `inbox` synchronously by the MCP tool; delivery = enqueue a turn
  on the target bot if `wake=true`).
- If the turn ended with `ask_owner` pending, the bot is marked `waiting` and
  no queued inputs are delivered until the answer arrives (answers are inputs).

### 3.4 Failure classification and failover

Classify `stderr`/`result.error`/exit code into:
- `auth_stale` — matches the known text `organization has disabled Claude
  subscription access` or `Not logged in`/`login` variants → mark the profile
  `needs_login`, notify owner with a **Relogin** button (§8), retry the turn
  once on another healthy profile.
- `rate_limited` — usage/rate limit text → put profile in cooldown until the
  cached `resets_at` (or 30 min), retry once on another profile.
- `transient` — network/5xx → retry once, same profile, after 10 s.
- `fatal` — anything else → post the error summary to the topic, mark turn
  failed, dequeue.

A retried turn reuses the same session UUID: because profiles share
`projects/` (§4) the other account can resume the same conversation.

## 4. Profiles: selection and shared sessions

- `pickProfile()` (already implemented) chooses per **turn**: lowest cached
  5-hour utilization, tie-break fewer working bots, then name; excludes
  profiles in cooldown or `needs_login`.
- **Shared sessions**: all profiles of an instance point their `projects/` at
  the same directory (`<data_dir>/projects`, symlinked into each config dir by
  `ccc` when a profile is added; for the implicit `~/.claude` profile the
  instance symlinks the other direction and documents it). Transcripts are
  plain files, so any account resumes any session. `jobs/`, `daemon/` and
  credentials stay per profile.
- `/account` (§8) is the only UI for profiles; the `ccc profile …` CLI remains
  as the underlying functions.

## 5. Data model (SQLite via GORM, `<data_dir>/ccc.db`, WAL, FK on)

```
bots        id, name (unique), topic_id, role (text), cwd, session_id, status (idle|running|waiting|disabled),
            created_at, archived_at, parent_bot_id (for spawned workers)
turns       id, bot_id, session_id, profile, source (user|bot|schedule|watch|system), input (text),
            output (text), status (queued|running|done|failed), stop_reason, error_class,
            started_at, ended_at, usage_json
inbox       id, to_bot_id, from_bot_id (nullable = owner/system), text, wake (bool), delivered_at, turn_id
memories    id, scope (user|project|bot), scope_key (''|project path|bot id), key, text,
            created_by_bot_id, created_at, updated_at        -- unique(scope, scope_key, key)
projects    id, path (unique), name, description, stack, deploy_notes, updated_at
watches     id, bot_id, name, command, interval_s, last_hash, last_output, last_run_at, enabled
schedules   id, bot_id, fire_at, note, recurring_cron (nullable), fired_at
questions   id, bot_id, turn_id, question, options_json, answer, asked_message_id, answered_at
access      telegram_user_id (pk), display, state (pending|approved|blocked), pair_code, code_expires_at
settings    key (pk), value                                  -- instance settings edited from Telegram
```

Existing `config.json` (bot token, group id, profiles) stays as bootstrap
config; everything runtime lives in SQLite. The v2 `sessions` map and the
JSONL ledger are not migrated (v3 is a fresh start; document it).

## 6. MCP server (`ccc mcp`)

Stdio JSON-RPC MCP server, one process per turn, spawned by Claude Code from
the inline `--mcp-config`. It opens the same SQLite file and knows the calling
bot/turn from its flags. Tools (all bots get all of them):

| Tool | Input | Behavior |
|---|---|---|
| `remember` | `scope` (user\|project\|bot), `key`, `text`, `project_path?` | Upsert a memory. `bot` scope is implicitly this bot. |
| `recall` | `query`, `scope?`, `limit?` | Full-text (SQLite FTS5) search over memories visible to this bot: all `user`, all `project`, own `bot`. Returns key+text+scope. |
| `forget` | `scope`, `key`, `project_path?` | Delete one memory. |
| `list_bots` | — | Names, roles, status, topic links of all live bots. |
| `send_to_bot` | `bot`, `text`, `wake` (default true) | Append to `inbox`; mirrored into both topics as `🤝 <from> → <to>: …`. Delivery happens post-turn (§3.3). |
| `notify_owner` | `text`, `urgency` (normal\|urgent) | Post in this bot's topic mentioning the owner; `urgent` also DMs the owner. |
| `ask_owner` | `question`, `options?` (≤4 strings) | Post question with inline buttons (or free text if no options). Returns immediately with `{"status":"asked"}`; the bot should end its turn. The answer arrives as the next input (`source=user`, prefixed `Answer to "<question>": …`). |
| `update_instructions` | `role` | Replace this bot's `role`; echo the new text into the topic. `/role` does the same from Telegram. |
| `watch` | `name`, `command`, `interval_s` (≥60) | Register a deterministic watch (§7). `unwatch(name)`, `list_watches()`. |
| `schedule_wakeup` | `in_seconds` or `at` (RFC3339), `note`, `cron?` | Self-wakeup (§7). `cancel_schedule(id)`. |
| `spawn_bot` | `name`, `role`, `cwd?`, `first_message?` | Create a child bot + topic; `parent_bot_id` = this bot; the child reports back with `send_to_bot(parent)`. |
| `archive_bot` | `bot?` (default self) | Close the topic (Telegram close, not delete), mark archived. |
| `get_project` / `set_project` | `path`, fields | Read/update the project registry. |
| `send_file` | `path`, `caption?` | Send a file into this bot's topic (≤50 MB; larger → existing relay if kept). |

All tools validate the calling bot from the `--bot` flag; tool inputs coming
from the model are data, never instructions to ccc.

## 7. Scheduler and watch engine

One goroutine in `ccc listen`:
- **Watches**: every `interval_s`, run `command` in the bot's cwd with the
  instance env (no model involved). Hash stdout; if changed since `last_hash`,
  enqueue a turn on the bot with `source=watch` and an input containing the
  watch name, the previous and new output (diffed, truncated to ~8 KB). Zero
  tokens while nothing changes.
- **Schedules**: enqueue a turn with `source=schedule` and the note when
  `fire_at` passes; recurring via cron expression.
- **Doctor loop** (every 15 min): `claude auth status --json` per profile
  (exit code 1 = logged out — verified), disclaimer check, usage cache read.
  Transitions to `needs_login` → owner notification with **Relogin** button.

## 8. Telegram UX

### Conversation
- Plain text in a bot's topic → input for that bot. No `/new` needed.
- Plain text in the group root (General) → creates a new bot: topic named from
  the first line, empty role, first message dispatched. `/bot <name> [role]`
  does the same explicitly.
- Photos/documents → saved into the bot's workspace `inbox/`, path passed in the
  message. Voice → transcribed if the `voice` build is present, else the file
  path is passed (keep the existing whisper integration).
- `ask_owner` → inline buttons `q:<question_id>:<option_idx>`; tapping edits the
  message with ✓ and enqueues the answer. Free-text answers: replying to the
  question message counts as the answer.
- Bot→bot traffic is visible in both topics (`🤝`).

### Commands
| Command | Where | Effect |
|---|---|---|
| `/role [text]` | topic | Show or set the bot's role. |
| `/new` | topic | Rotate the session (fresh conversation, memory kept). |
| `/stop` | topic | Kill the running turn (SIGTERM the `claude` process), drop the queue. |
| `/cwd [path]` | topic | Show or set the bot's working dir. |
| `/memory [query]` | topic | List/search memories visible to this bot; `/forget <scope> <key>`. |
| `/watches`, `/schedules` | topic | List and cancel. |
| `/bots` | anywhere | Table of bots, status, last activity. |
| `/account` | anywhere | Status card per profile with buttons; subcommands `status`, `add <name>`, `login <name>`, `remove <name>`, `default <name>`. |
| `/model [name]` | anywhere | Show/set the instance model. |
| `/access` | anywhere | Pairing/allowlist management (below). |
| `/status` | anywhere | Instance health: profiles, running turns, queue, doctor findings. |

### Account management from Telegram (login without a terminal)
`/account add <name>` (or **➕** button):
1. ccc creates `<data_dir>/profiles/<name>` (config dir), symlinks `projects/`,
   registers the profile.
2. Runs `claude auth login` in a **pseudo-terminal** (`github.com/creack/pty`)
   with `claudeEnv(profile)`. Parses the login URL from the PTY output and posts
   it. The user opens it on the phone with the right account and pastes the code
   back into the chat; ccc writes it to the PTY. Success is confirmed by
   `claude auth status --json` (email shown).
3. Disclaimer: runs `claude --dangerously-skip-permissions` in the PTY, detects
   the prompt, answers it, verifies `skipDangerousModePermissionPrompt` in
   `settings.json` (detector already implemented in `profiles.go`).
4. Times out after 10 min; the partial profile is removed.
`/account login <name>` runs steps 2–3 only. The exact PTY strings must be
verified against 2.1.259 by the implementer and kept in one place.

### Access control (copied from the official Telegram channel plugin)
- Owner = the Telegram user id from bootstrap config; always allowed.
- Unknown DM → 6-hex pairing code (1 h TTL, ≤3 pending); owner approves with
  `/access pair <code>` (or a button in the owner's DM). Approved users may talk
  to bots in the group; only the owner can use `/account`, `/access`, `/model`.
- Every inbound update from a non-approved user is dropped silently after the
  pairing reply. Callback queries are gated the same way.

## 9. System prompt and context envelope

**System prompt** (`--system-prompt`, rendered once per session; a change of
role or template rotates the session):

```
You are <name>, a bot in Jairo's ccc team. Role: <role>.
You run on machine <hostname>, working dir <cwd>. Today is <date>.
Tools: you have the ccc MCP tools (memory, messaging, scheduling, watches,
spawning) plus the standard tools (Bash, Read, Edit, …) with full permissions.
Other bots: <name — role> list.
Rules: … (owner escalation, when to remember, never print secrets, keep
replies short for chat, prefer ask_owner over guessing on architecture…)
```

**Envelope** (prepended to every input, because system-prompt changes are
ignored on resume until compaction):

```
<context>
recent user memories (top 10 by recency/relevance to the message)
project memories for cwd (if any)
own bot memories (top 10)
pending inbox summary (N messages from X)
</context>
<message source="user|bot:<name>|watch:<name>|schedule">…</message>
```

Keep the envelope under ~4 KB; `recall` exists for everything else.

## 10. Isolation from Claude Code defaults (implementer verifies each)

Must be off for bots: auto-memory, `CLAUDE.md` auto-discovery, project/local
settings, user plugins/skills (unless `--setting-sources user` is required for
auth — verify), slash-command expansion (`--disable-slash-commands`), Remote
Control. Must be on: MCP tools from ccc only (`--strict-mcp-config`), standard
tools, bypass permissions. Record the final flag set in `runner.go` with a
comment per flag citing the reason.

## 11. Legacy removal plan

Phase 2 replaces `listen`'s core. Delete when the new runner passes E2E:
`poller.go`, `session.go`, `sessionlabel.go`, `hooks.go` (hook-question),
`ledger.go`, the bg-agent parts of `agents.go` (keep `claudeEnv`, profile
helpers, `parseBgShortID` only if still used), `live_test.go`. Keep
`telegram.go`, `relay.go` (file relay), `whisper*.go`, `service.go`,
`profiles.go`, `profilecmd.go`, `config.go`.

## 12. Security posture

- Bots run with bypass permissions on the owner's machine: **the chat is the
  trust boundary**. Access control (§8) is therefore mandatory, not optional.
- Secrets reach bots only via `env_passthrough`; ccc never posts env values,
  tokens, or credential file contents to Telegram; `send_file` refuses paths
  under config dirs and `<data_dir>/profiles`.
- Tool inputs and Telegram text are data. Pairing is never approved because a
  message asked for it.

## 13. Delivery plan

- **2a — runner core**: SQLite/GORM schema, `runner.go` (§3), profile failover,
  `ccc mcp` with `remember/recall/forget/list_bots/send_to_bot/notify_owner/
  ask_owner/update_instructions/send_file`, Telegram conversation flow (§8
  "Conversation" + `/role /new /stop /cwd /bots /memory /status`), envelope,
  system prompt, progress rendering. E2E: two bots talking to each other via
  `send_to_bot`, an `ask_owner` round trip, a failover forced by disabling a
  profile.
- **2b — automation & accounts**: watches, schedules, `spawn_bot/archive_bot`,
  project registry, doctor loop, `/account …` with PTY login and disclaimer,
  `/access` pairing, `/model`, legacy removal (§11), README rewrite.

Each phase: `go build && go vet && go test && gox check` green, conventional
commits, no push until Jairo says so.
