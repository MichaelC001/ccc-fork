# ccc v3 — design

Status: implemented (Phases 2a and 2b, 2026-09-14). This document is the
specification the implementation follows. When code and this document disagree,
fix one of them in the same change. Everything below describes what ccc v3
actually does; §14 lists where the built thing knowingly departs from the
original plan, and why.

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
          --setting-sources ''                         # no settings AND no CLAUDE.md (§14.1)
          --disable-slash-commands                     # no user/plugin skills (§14.1)
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
  auto-memory. `--bare` is NOT an option (it disables OAuth). Verified: only
  the EMPTY `--setting-sources` achieves this (§14.1); `runner.go` carries the
  probe and one comment per flag.
- Environment: `claudeEnv(profile)` whitelist only (already implemented) plus
  the instance's `env_passthrough` list (e.g. `GH_TOKEN`, `SLACK_USER_TOKEN`,
  `LINEAR_API_KEY`). Never inherit `CLAUDE*`/`ANTHROPIC*` from the parent.
- The MCP config points at `ccc mcp --bot <id> --turn <id>` (stdio). ccc is
  therefore both the Telegram client and an MCP server binary (§6).
- Streaming: `--output-format stream-json`. `--include-partial-messages` is NOT
  used: per-message granularity is enough for the progress line.

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
  on the target bot if `wake=true`, labelled with the sender). `wake=false`
  rows are not delivered as turns: they are summarized in the next envelope.
- If the turn ended with `ask_owner` pending, the bot is marked `waiting` and
  no queued inputs are delivered until the answer arrives (answers are inputs).
- If `update_instructions` changed the role during the turn, rotate the session
  now that the turn has recorded its id (§14.2).

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

- `pickProfile()` chooses per **turn**: lowest cached 5-hour utilization,
  tie-break fewer turns currently running on that account (read from
  `turns.status = running`, §14.6), then name; excludes profiles in cooldown or
  `needs_login`.
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
access      telegram_user_id (pk), display, state (pending|approved|blocked), pair_code, code_expires_at,
            replies (how many times ccc has answered this stranger, §14.7)
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
| `set_name` | `name`, `emoji?` | Rename this bot: validate (§8 `/name`), update `bots.name`, rename the forum topic and, when `emoji` is one Telegram allows, set the topic icon. Rotates the session (§14.14). `/name` does the same from Telegram. |
| `watch` | `name`, `command`, `interval_s` (≥60) | Register a deterministic watch (§7). `unwatch(name)`, `list_watches()`. |
| `schedule_wakeup` | `in_seconds` or `at` (RFC3339), `note`, `cron?` | Self-wakeup (§7). `cancel_schedule(id)`. |
| `spawn_bot` | `name`, `role`, `cwd?`, `first_message?`, `emoji?` | Create a child bot + topic (icon from `emoji`); `parent_bot_id` = this bot; the child reports back with `send_to_bot(parent)`. |
| `archive_bot` | `bot?` (default self) | Close the topic (Telegram close, not delete), mark archived. |
| `get_project` / `set_project` | `path`, fields | Read/update the project registry. |
| `send_file` | `path`, `caption?` | Send a file into this bot's topic (≤50 MB; larger → existing relay if kept). |

All tools validate the calling bot from the `--bot` flag; tool inputs coming
from the model are data, never instructions to ccc.

Topic icons are not free-form: `editForumTopic` only accepts a custom-emoji id
out of `getForumTopicIconStickers`. ccc caches that list (memory + `settings`,
refreshed daily, §14.16) and puts the allowed emoji into the `set_name` and
`spawn_bot` tool descriptions and the system prompt, so the model picks one that
exists. An emoji outside the set leaves the icon untouched and the tool result
says which emoji were available.

## 7. Scheduler and watch engine

One goroutine in `ccc listen`:
- **Watches**: every `interval_s` (floor: 60 s), run `command` in the bot's cwd
  with the instance env (no model involved). The FIRST run only records a
  baseline and wakes nobody; changing a watch's command resets that baseline.
  Hash stdout; if changed since `last_hash`,
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
  question message counts as the answer, and so does ANY text sent while the bot
  is parked `waiting` (a reply-to takes priority when both apply, §14.3).
- Bot→bot traffic is visible in both topics (`🤝`).
- Renaming a topic in Telegram itself renames the bot: the `forum_topic_edited`
  service message is validated like `/name` and, when it passes, `bots.name`
  follows the title (§14.15).

### Commands
| Command | Where | Effect |
|---|---|---|
| `/role [text]` | topic | Show or set the bot's role. |
| `/name [text] [emoji]` | topic | Show or set the bot's name: renames the forum topic, sets its icon and rotates the session (§14.14). |
| `/new` | topic | Rotate the session (fresh conversation, memory kept). |
| `/stop` | topic | Kill the running turn (SIGTERM the `claude` process), drop the queue. |
| `/cwd [path]` | topic | Show or set the bot's working dir. |
| `/memory [query]` | topic | List/search memories visible to this bot; `/forget <scope> <key>`. |
| `/watches`, `/schedules` | topic | List and cancel. |
| `/bots` | anywhere | Table of bots, status, last activity. |
| `/account` | anywhere | Status card per profile with buttons; subcommands `status`, `add <name>`, `login <name>`, `remove <name>`, `default <name>`. |
| `/model [name]` | anywhere | Show/set the instance model. |
| `/access` | anywhere | Pairing/allowlist management (below). Owner only. |
| `/watches`, `/schedules` | topic | List and cancel (also listed above). |
| `/setgroup` | group | Bind the instance to this forum group. Owner only, and the headless alternative to `ccc setgroup`. |
| `/status` | anywhere | Instance health: profiles, running turns, queue, doctor findings. |

### Account management from Telegram (login without a terminal)
`/account add <name>` (or **➕** button):
1. ccc creates `<data_dir>/profiles/<name>` (config dir), symlinks `projects/`,
   registers the profile.
2. Runs `claude auth login` in a **pseudo-terminal** (`github.com/creack/pty`)
   with `claudeEnv(profile)` **plus browser suppression** (§14.8): a directory
   of no-op `open`/`xdg-open` shims first on `PATH` and `$BROWSER` pointed at
   one of them. Parses the login URL from the PTY output and posts it. The user
   opens it on the phone with the right account and pastes the code back into
   the chat; ccc writes it to the PTY. Success is confirmed by
   `claude auth status --json`, never by the TUI.
3. Disclaimer: runs `claude --dangerously-skip-permissions` in the PTY, detects
   the prompt, answers it, verifies `skipDangerousModePermissionPrompt` in
   `settings.json` (detector already implemented in `profiles.go`).
4. Times out after 10 min; the partial profile is removed.
`/account login <name>` runs steps 2–3 only. Every PTY string and pattern lives
in `ptyflow.go`, each with a note on how it was verified against 2.1.270
(§14.9). Select lists are answered by reading the option number off the screen,
never by assuming a position.

### Access control (copied from the official Telegram channel plugin)
- Owner = the Telegram user id from bootstrap config; always allowed.
- Unknown DM → 6-hex pairing code (1 h TTL, ≤3 pending, ≤2 replies per stranger
  and then silence); owner approves with `/access pair <code>` (or a button in
  the owner's DM). Approved users may talk to bots in the group; only the owner
  can use `/account`, `/access`, `/model` and `/setgroup`.
- Every inbound update from a non-approved user is dropped silently after the
  pairing reply. Messages, EDITS and callback queries are gated the same way; an
  unknown user in the GROUP gets no reply at all, because answering there would
  let anyone who finds the group make the bot talk.
- Until `chat_id` is configured there is no owner, so nobody is allowed.

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
onboarding instruction (only while the bot has no role, §14.17)
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
settings, user plugins/skills, slash-command expansion. All of these fall to
`--setting-sources ''` plus `--disable-slash-commands`; auth is unaffected,
because OAuth lives in the keychain/credentials and not in `settings.json`.
Must be on: MCP tools from ccc only (`--strict-mcp-config`), standard tools,
bypass permissions. The final flag set is in `runner.go` with a comment per flag
citing the reason and the probe that established it.

## 11. Legacy removal plan

Done in Phase 2b. Deleted: `poller.go`, `poller_naming_test.go`, `session.go`,
`sessionlabel.go`, `hooks.go`, `ledger.go`, `live_test.go`, and from
`agents.go`/`helpers.go`/`commands.go`/`main.go` everything that served
background agents, the transcript scraper, the JSONL ledger or the v2 CLI
(`ccc`, `ccc -c`, `ccc start`, `ccc hook-question`, the ccc-send skill
installer). `Config` lost `sessions`, `projects_dir`, `away`, `oauth_token` and
`otp_secret`; `loadConfig` ignores unknown keys, so a v2 config still starts a
v3 instance and the first save rewrites it clean. Kept: `telegram.go`,
`relay.go`, `whisper*.go`, `service.go`, `profiles.go`, `profilecmd.go`,
`config.go`, plus `claudeBin`/`runClaudeOutput` out of `agents.go`.

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
- **2b — automation & accounts** (done): watches, schedules,
  `spawn_bot/archive_bot`, project registry, doctor loop, `/account …` with PTY
  login and disclaimer, `/access` pairing, `/model`, `/setgroup`, headless
  bootstrap (`ccc config set`, systemd user unit, `make build-linux`), legacy
  removal (§11), README rewrite.

Each phase: `go build && go vet && go test && gox check` green, conventional
commits, no push until Jairo says so.

## 14. Deviations from the original plan

Everything here is a place where building ccc taught us something the plan got
wrong, or where the plan was silent and a decision had to be made. Each entry
says what changed and why.

**14.1 `--setting-sources ''`, not `--setting-sources user`.** §3.1 originally
asked for `user`, on the assumption that `--system-prompt` already suppressed
`CLAUDE.md`. It does not. A probe against 2.1.270 — a workspace holding a
`CLAUDE.md` with a token, and a `~/.claude/CLAUDE.md` holding another, asking
the model which it could see — gave:

```
no flags               -> project=yes user=yes
--system-prompt only   -> project=yes user=yes   (!)
--setting-sources user -> project=no  user=yes
--setting-sources ''   -> project=no  user=no
```

Only the empty value loads no `CLAUDE.md` at all, so `user` would leak the
owner's personal memory into every bot. Auth is unaffected. `--disable-slash-
commands` was added alongside it so no installed skill can steer a bot.

**14.2 `update_instructions` rotates the session AFTER the turn.** A role change
cannot take effect mid-conversation (the system prompt is recorded per
conversation, 14.5), and rotating during the turn would lose the session id the
turn is about to write. The runner compares the role before and after and
clears `session_id` once the turn has been persisted.

**14.3 Any text to a `waiting` bot counts as the answer.** §8 only specified
"replying to the question message". In practice people answer without using
reply-to, and the bot is parked either way. Reply-to still takes priority when
both could apply, so answering an older question explicitly still works.

**14.4 `linkSharedProjects` only creates symlinks.** §4 was silent about an
existing `projects/`. Replacing one would destroy real transcript history, so
ccc creates the link when the path is free and otherwise leaves it exactly as
it is — including the reverse link for the implicit `~/.claude` profile.

**14.5 The system prompt is snapshotted per conversation, and
`--system-prompt-snapshot off` does not change that.** Verified: a resumed
session whose launch passed a DIFFERENT `--system-prompt` still answered with
the original prompt's secret word, with the flag explicitly set to `off`. This
is what makes §9's envelope load-bearing and what makes `/role` rotate the
session.

**14.6 `WorkingAgents` comes from `turns.status = running`.** §4 inherited the
v2 idea of counting "working background agents" from the fleet view. v3 has no
fleet; one running turn is one `claude -p` process, so the turn table is both
cheaper and exactly right.

**14.7 `access` has a `replies` column.** §5's column list has nowhere to record
"at most two replies to a stranger, then silence", which §8 requires. One
integer per row.

**14.8 The PTY login flow suppresses the browser.** Not in the plan, and found
the hard way: `claude auth login` shells out to `open`/`xdg-open` (and honours
`$BROWSER`), so the first string-capture probe opened a real browser tab on the
Mac pointing at a `localhost` callback nothing was listening on. Every PTY flow
now runs with a directory of no-op shims first on `PATH` and `$BROWSER` pointed
at one of them. The login URL belongs in Telegram and nowhere else — the VM is
headless, and hijacking the owner's browser on the Mac is worse than useless.

**14.9 The disclaimer strings were read from the binary, not captured.** The
login prompts were captured verbatim from a throwaway config dir under
`/private/tmp` (process killed, directory deleted, no code entered, no login
completed, `~/.claude` and the Keychain untouched). The bypass-permissions
disclaimer only appears once a config dir is logged in, which that constraint
forbids, so its strings were extracted from the 2.1.270 binary instead — the
same technique that produced `bypassDisclaimerMsg`. Because neither probe pins
the option ORDER, the driver reads the numbered list off the screen and answers
with the number beside the label it wants, and success is always verified
against real state (`claude auth status --json`, `bypassAccepted`).

**14.10 A waking inbox message becomes a turn.** §3.3 says delivery "= enqueue a
turn on the target bot"; Phase 2a only kicked the target's queue, which had
nothing in it, so `send_to_bot` never actually reached anybody. Phase 2b creates
the queued turn, labelled with the sender. This is what makes `spawn_bot` with a
`first_message` — and the child's report back — work.

**14.11 A watch's first run is a baseline.** §7 did not say what happens on the
very first run, when `last_hash` is empty. Treating that as a change would wake
the bot for "the watch exists", so the first run records the hash silently.
Changing a watch's command resets the baseline for the same reason.

**14.12 `/model` does not rotate sessions.** Unlike `/role`, `--model` is passed
on every turn including resumes, so a model change takes effect immediately and
there is nothing to rotate.

**14.13 Headless bootstrap.** §8 assumed `ccc setup`'s interactive Telegram
loop. A VM has no terminal to run it in, so `ccc config set <key> <value>` sets
every bootstrap key non-interactively and `/setgroup` binds the forum group from
Telegram. `ccc install` writes a systemd **user** unit whose only `Environment=`
lines are the `env_passthrough` names that are actually set.

**14.14 Renaming rotates the session, like `/role`.** The bot's name is in the
system prompt (`You are <name>, …`, §9), and the system prompt is recorded per
conversation (14.5), so a rename would otherwise leave the model answering to
its old name until the next compaction. `/name`, `set_name` and the topic-title
sync therefore all clear `session_id` — memories are kept, exactly like `/role`,
and the confirmation message says so. A `set_name` call made DURING a turn is
rotated by the same post-turn check as `update_instructions` (14.2), which also
repairs the id a fresh session wrote back after the tool cleared it. The OTHER
bots keep their sessions: their system prompt roster goes stale, which is what
`list_bots` is for, and `send_to_bot` resolves names against the live table.

**14.15 The topic title and `bots.name` are kept in sync in both directions.**
The name is unique and addressable (`send_to_bot`, `spawn_bot`, `list_bots`), so
it cannot be a free-text label; the topic title is the same string to the person
reading the chat. `/name` and `set_name` rename the topic; a rename made in
Telegram arrives as a `forum_topic_edited` service message and renames the bot.
A title that fails validation (taken, empty, too long) is NOT renamed back —
that would fight the person renaming it, and could loop — the old name is kept
and the topic is told why.

**14.16 Topic icons come from a cached sticker set.** Forum topics cannot take
an arbitrary emoji: `editForumTopic` wants an `icon_custom_emoji_id` from
`getForumTopicIconStickers`. The list is identical for every bot and changes
rarely, so ccc caches it in memory and in `settings` and refreshes it once a
day; when the fetch fails the stale list is used, because a missing icon is much
cheaper than a failed rename. Matching is exact first, then the same emoji
ignoring variation selectors and skin tones; no match leaves the icon alone and
reports the available emoji instead of guessing a different icon.

**14.17 A role-less bot is onboarded from the envelope, not the system prompt.**
A bot created from a line in General starts with an empty role, and a generic
assistant is not what the owner asked for. Every turn of a role-less bot carries
an instruction to introduce itself, ask what it is for, and then store the answer
with `update_instructions` and pick a name and icon with `set_name`. It lives in
the envelope because the system prompt is frozen per conversation (14.5): from
there it could not disappear the moment the role is set, which is exactly when
it has to.
