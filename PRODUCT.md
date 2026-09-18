# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Static HTML and CSS in `docs/`, deployed by `.github/workflows/pages.yml` on push to `main` (path `docs/**`) to GitHub Pages at https://kidandcat.github.io/ccc/. No framework, no Go server, not Fly. Inferred from the landing brief (2026-09-18) and the existing workflow; labeled inferred where the brief was the source.

## Users

Primary: a developer who already runs (or will run) coding CLIs on a machine they own, and who wants a **Telegram dispatcher** for AI sessions — one DM to talk to, backend workers that do the work. They are often away from a desk (phone, couch) and do not want a web dashboard, a Slack app, or a forum of session topics.

Secondary: someone evaluating the public OSS repo (`kidandcat/ccc`) before cloning. They need an accurate picture, not a sales fiction.

## Product Purpose

**ccc** is coding sessions in Telegram. The bot's 1:1 DM is **Chief**, the dispatcher: you talk to it, it sees live sessions, and it can start a backend worker (`spawn_session`) or message one (`tell_session`). Sessions live in the backend — no Telegram topic. The owner never writes into a session chat. Workers report only to Chief (`report_to_chief`); the owner does not see the transcript. Chief posts a short DM summary.

Success for this landing: a first-time visitor understands that model in seconds, believes the product is self-hosted OSS (not a hosted chat SaaS), and goes to https://github.com/kidandcat/ccc.

## Positioning

The mechanism a neighboring product could not copy without becoming ccc: **one Telegram DM is the dispatcher; sessions are backend workers with no chat of their own.** Quiet owner UX (`ask_owner` buttons, vault secrets the model never reads, watches that cost nothing until output changes, routines that fire as fresh workers). Engines are interchangeable runners (Claude Code default; Grok Build, Antigravity, Codex) behind the same envelope.

Not: a Claude Code plugin, a Slack bot, a web agent dashboard, or Claude background agents / `claude attach` (explicitly dropped in v3).

## Operating Context

- One `ccc listen` process on one machine, bound to one Telegram bot token. Owner `chat_id` is the access-control root.
- Default-deny by Telegram user id. Approved users can talk in the DM; `/account`, `/access`, `/model`, `/secret` stay owner-only.
- Phone client (MIT, public): [ccc-app](https://github.com/kidandcat/ccc-app) — `ccc pair`, paste the URI. Default hub `wss://hub.mentasystems.com` is a free encrypted relay that cannot read chats. `ccc hub` runs your own.
- Bootstrap is headless: BotFather token, `ccc config` / `ccc install` (launchd / systemd --user). Login URLs go to Telegram; ccc never opens a browser on the machine.
- Implementation spec (not visual): [`docs/DESIGN.md`](docs/DESIGN.md). **Do not overwrite that file with a visual design system.** Visual DESIGN.md, if written, lives at the repo root.

## Capabilities and Constraints

Confirmed (README / `docs/DESIGN.md`):

- Chief 60s cap; longer work goes to a session. If the cap fires and Chief does not spawn, ccc starts the session itself. `/session <prompt>` starts a worker without Chief.
- Idle sessions waiting on the owner wake Chief every 10 minutes (inbox, not a chat ping).
- Tools sessions actually have: `remember` / `recall` / `forget`; `notify_owner` / `ask_owner`; `watch` / `schedule_wakeup` / `set_routine`; `run_background` / `run` with vault inject; `secrets_list` / `secrets_delete` (no `secrets_get`); Chief-only `spawn_session` / `tell_session`; workers-only `report_to_chief`; `send_file`; `get_project` / `set_project`; `set_name`; `archive_bot`.
- Engines: Claude Code, Grok Build, Antigravity, Codex. Failover stays inside the same engine.
- Owner vault (`/secret add`); values never shown; sessions inject via env/stdin.
- MIT license. Go 1.25+.

Landing constraints (brief, 2026-09-18):

- Copy in English. Follow README facts; **do not invent features.**
- Link to https://github.com/kidandcat/ccc. No secrets, no personal phone numbers.
- The incumbent `docs/index.html` is a v2-era landing (forum topics, `claude attach`, “background agents”, invented “interactive approvals”). It is **anti-reference** for product truth. Replace it; do not polish those claims.

Undecided / do not fabricate: user counts, testimonials, pricing (there is none — self-hosted), benchmarks, screenshots of a real production DM.

## Brand Commitments

- Product name is **ccc** (lowercase in running text). No expansion (not “Crew Command Center”).
- Voice in the README: precise, second-person, short. No hype, no “join developers who…”. Landing copy should stay in that register even when it is friendlier than the README.
- Telegram is the interface, not the brand color by default.
- MIT. Public repo. Phone app is a separate public repo.
- **Landing visual world (standing preference, 2026-09-18):** category canon — a typical coding-tool product site, sitting alongside Cursor, Claude, and Codex. Played straight at their craft level. No radio / ATC / sewing (or any other governing metaphor). No neo-terminal costume. No irony or smuggled quirk.

## Evidence on Hand

- [`README.md`](README.md) — public product facts.
- [`docs/DESIGN.md`](docs/DESIGN.md) — implementation specification (v3).
- [`docs/index.html`](docs/index.html) + [`docs/style.css`](docs/style.css) — incumbent Pages landing; visually a dark neo-terminal with Outfit/IBM Plex; factually stale. Use as anti-reference for claims.
- Live URL already: https://kidandcat.github.io/ccc/ (workflow from `docs/`).
- No photography, no logo lockup, no testimonials. Do not invent them.

## Product Principles

1. **The DM is Chief.** If a sentence implies the owner chats with a worker, it is wrong.
2. **Facts over atmosphere.** Friendliness is tone and craft, not extra capabilities.
3. **Self-hosted is the product.** The public hub is an encrypted pipe, not a cloud that runs your agents.
4. **Quiet by default.** Progress is silent; the ping is the answer; `ask_owner` is how decisions happen.
5. **One instance, one bot.** Do not draw a mesh of bots talking to each other.

## Accessibility & Inclusion

No product-specific legal standard was set. The landing must remain usable: real headings, keyboard-focusable controls, contrast that holds on the chosen ground, no information in color alone. English-only copy is a brief constraint, not a claim that other languages are unsupported in the binary.
