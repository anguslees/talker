# Talker

A personal, always-on voice companion. It holds a low-latency spoken conversation
with Gemini Live, hands real work to a [Hermes](https://github.com/NousResearch/hermes-agent)
gateway, and tells you when things finish — during a natural pause, without you
leaving whatever you were doing.

```sh
go install github.com/anguslees/talker@latest
```

The UI is embedded in the binary; see [Running](#running) for the three
environment variables it needs.

```
Mac browser  <══ audio (direct WebSocket) ══>  Gemini Live
     │
     └── tool calls / results / events ──>  Talker (Go, on your devbox)  ──>  Hermes API
```

Audio never touches your devbox. The Go server mints short-lived, single-use
Gemini tokens whose configuration is locked server-side, executes the tools the
model selects (through Google ADK for Go), keeps a durable ledger of background
tasks, and streams task/notification state to the browser over a separate
JSON-only control socket.

## What it does

- **Continuous, bidirectional speech** — no push-to-talk. Gemini's native VAD
  handles turn-taking; barge-in interrupts playback immediately.
- **Background work via Hermes** — "look into X", "search for Y", "let me know
  when that finishes". Tasks are admitted with idempotency keys, monitored via
  SSE + polling, and survive browser disconnects and Talker restarts.
- **Quiet-break announcements** — completions queue up and are spoken only when
  both you and the model have been quiet for a configurable period. Nothing
  routine interrupts you.
- **Honest acknowledgement** — a notification is only marked heard after it was
  spoken to completion with audio actually played. Interrupted announcements
  stay in the inbox.
- **Progressive answers** — tools are `NON_BLOCKING`, so the model can say
  "I'll get on that" and continue talking; results arrive `WHEN_IDLE`.
- **Expressive, varied voice** — temperature 1.1 by default, with delivery-cue
  prompting for pacing and tone.
- **Extensible events** — any script can `POST /api/events` to have something
  announced (CI finished, deploy done, …).

## Requirements

- Go 1.26+
- A [Gemini Developer API](https://ai.google.dev/) key, kept on the devbox only
- A running Hermes gateway with the **API Server adapter** enabled
  (`API_SERVER_ENABLED=true`, `API_SERVER_KEY=…`; default port 8642). Hermes
  must advertise durable run idempotency in `/v1/capabilities`, which recent
  releases do.
- A browser on your laptop with a microphone (Chrome/Edge/Safari recent)

## Running

On the devbox:

```sh
export GEMINI_API_KEY=…            # or GEMINI_API_KEY_FILE=/path/to/key
export HERMES_URL=http://127.0.0.1:8642
export HERMES_API_KEY=…            # the Hermes API_SERVER_KEY
talker
```

Talker listens on `127.0.0.1:8080` by default. Reach it from your laptop either way:

- **Authenticating HTTPS reverse proxy** in front of Talker (the proxy must pass
  WebSocket upgrades for `/api/control` and, if it rewrites `Host`, set
  `X-Forwarded-Host`). Open the proxy's URL.
- **SSH tunnel:** `ssh -N -L 8080:127.0.0.1:8080 devbox`, then open
  <http://localhost:8080>.

Both are secure contexts (HTTPS or `localhost`), which the browser requires for
microphone access. Click **Start talking** once (browsers require a gesture to
start audio); after that, just talk.

Talker performs no authentication of its own: anyone who can reach the port can
drive your Hermes agent and mint Gemini tokens billed to your key. Keep the
default loopback bind unless something authenticated sits in front.

Verify credentials and model access without a browser:

```sh
talker -check
# INFO Gemini Live check passed model=gemini-3.8-live setup=1.9s first_audio=750ms audio_bytes=132482
```

### Configuration

| Flag / env | Default | Purpose |
|---|---|---|
| `-listen` / `TALKER_LISTEN` | `127.0.0.1:8080` | HTTP listen address |
| `-model` / `TALKER_MODEL` | `gemini-3.8-live` | Gemini Live model |
| `-voice` / `TALKER_VOICE` | `Aoede` | Prebuilt voice |
| `-language` / `TALKER_LANGUAGE` | `en-US` | Speech language the model must stick to (BCP-47). Empty lets it auto-detect, which drifts at high temperature |
| `-temperature` / `TALKER_TEMPERATURE` | `1.1` | 0–2; higher = more varied |
| `-quiet-period` / `TALKER_QUIET_PERIOD` | `2s` | Silence required before announcing |
| `-speech-start` / `TALKER_SPEECH_START` | `low` | Gemini start-of-speech sensitivity. `low` ignores clicks and keyboard noise; `high` catches soft or brief speech at the cost of phantom turns |
| `-speech-prefix` / `TALKER_SPEECH_PREFIX` | `200ms` | Speech required before Gemini commits a turn. Included retroactively, so it never clips real onsets; transients shorter than this are ignored |
| `-instruction` / `TALKER_INSTRUCTION` | | Extra personality/preferences appended to the system prompt |
| `-state` / `TALKER_STATE` | `$XDG_STATE_HOME/talker/tasks.json` | Durable task/notification ledger (0600); `XDG_STATE_HOME` defaults to `~/.local/state` |
| `-hermes-url` / `HERMES_URL` | | Hermes API base, may include `/p/<profile>` |
| `HERMES_API_KEY` / `HERMES_API_KEY_FILE` | | Hermes bearer key (the gateway's `API_SERVER_KEY`) |
| `GEMINI_API_KEY` / `GEMINI_API_KEY_FILE` | | Server-side Gemini key |
| `TALKER_EVENT_TOKEN` | | Enables `POST /api/events` for external integrations |

See `.env.example`.

## Voice tools

The model can call these (all Go ADK `functiontool`s, `NON_BLOCKING`):

| Tool | Effect |
|---|---|
| `ask_hermes` | Ask a quick question and wait for the answer (a few seconds); optionally continue an existing session |
| `start_task` | Submit longer work as a background run; completion is announced at a quiet moment |
| `watch_task` | Adopt a run started elsewhere by its ID and announce its completion |
| `list_tasks`, `get_task` | Inspect tracked tasks (bounded previews / full result) |
| `stop_task`, `steer_task` | Ask Hermes to stop a run, or queue extra guidance for it |
| `answer_approval` | Resolve a Hermes tool-approval prompt with the user's explicit choice |
| `hermes_sessions`, `hermes_session` | Browse conversations from every source (CLI, chat apps, Talker) and read what one concluded |
| `hermes_status` | Reachability, version, and the model Hermes is currently routing to |
| `acknowledge_events` | Dismiss notifications the user says they've heard |

Tool calls the model raises *while reading a background announcement* are
treated as coming from untrusted data: read-only tools run, anything with side
effects is refused with an explanation the model can voice. The session keeps
going.

Deliberately absent: session rename/pin/fork/delete and model locking (voice is
a poor interface for housekeeping and a dangerous one for destructive actions),
and Hermes's own toolsets/skills (those are for Hermes's use, not Talker's).

## External events

```sh
curl -X POST localhost:8080/api/events \
  -H "Authorization: Bearer $TALKER_EVENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"CI finished","body":"main is green, 412 tests passed"}'
```

It appears in the Updates inbox and is spoken at the next quiet break.

## Security model

This is a single-user tool, but a few properties are deliberate:

- The Gemini API key and Hermes key never leave the devbox. The browser gets a
  one-use ephemeral token (30 min lifetime, 60 s to connect) with the **entire
  Live setup locked in** — the client cannot change model, tools, or prompt.
- Loopback bind by default; same-origin (honouring `X-Forwarded-Host`) is enforced on
  all mutating routes, so a proxy-authenticated session cannot be driven cross-site.
- The control socket accepts JSON only; audio frames close it.
- Result payloads are bounded before reaching the model or the browser.
- Stop/approve are never automatic; Hermes work is never restarted on the
  user's behalf after an uncertain admission.

## Development

```sh
go test -race ./...                 # Go: hermes client, task ledger, broker, control, orchestrator
node --test internal/web/*.test.js  # Browser: resampler, playback, Live protocol, lifecycle
```

Layout:

```
main.go                  wiring, -check
internal/config          flags/env
internal/live            ephemeral-token broker, Live setup, control WebSocket, -check
internal/orchestrator    Go ADK agent + functiontools; bounded JSON task views
internal/hermes          Hermes API Server client (runs, SSE, stop/steer/approval)
internal/tasks           durable ledger, monitors, notifications, Watch/Recent/Pending
internal/server          HTTP mux, same-origin + loopback guards, /api/*
internal/web             embedded UI: app.js (UI), live.js (Gemini + quiet gate),
                         audio.js (resampling, PlaybackQueue), mic-worklet.js,
                         playback-worklet.js (real-time drain on the audio thread)
```

## Known limitations

- Verified end-to-end with headless Chromium and against a live Hermes
  v0.21.4 gateway; real macOS microphone/echo behaviour is still untested.
- Gemini Live sessions are resumed through resumption handles on `GoAway`;
  after a small number of rotations the session ends and you press Start again.
- If the control socket to the devbox drops, voice ends (so speech is never
  disconnected from tools). Background tasks continue regardless.
- Browsers may throttle audio in background tabs or on sleep; keep the tab open
  and the laptop awake. The page requests a screen wake lock while connected.
