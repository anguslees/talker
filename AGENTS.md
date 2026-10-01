# Talker — notes for AI sessions

Personal voice assistant: browser ↔ Gemini Live for audio, Go server for tools
and durable task state, Hermes as the worker. Single user, runs on a devbox
behind an SSH tunnel or authenticating proxy. Read `README.md` for setup.

## Architecture decisions that are easy to regress

- **Audio never touches the Go server.** The browser opens its own WebSocket to
  Gemini using a server-minted ephemeral token; Go sees only tool calls, results
  and events over `/api/control` (JSON only — binary frames close it). Anything
  that routes PCM through Go is a latency regression, not a simplification.
- **The ephemeral token locks the entire Live setup.** `internal/live/token.go`
  sends `bidiGenerateContentSetup` with no `fieldMask`, so the browser cannot
  alter model/tools/prompt — and neither can it add a resumption handle after
  minting. Resumption works by POSTing `{handle}` to `/api/live/token` and
  using the returned setup verbatim. Change setup in `setup.go` only.
- **Gemini Developer API, not Vertex.** Vertex has no browser ephemeral tokens
  (verified 2026-09); switching backends means re-proxying audio.
- **Gemini has no playback backpressure.** Replies arrive ~5× faster than real
  time. Playback is an unbounded PCM queue drained on the audio render thread
  (`playback-worklet.js`), immune to background-tab timer throttling. Do not
  reintroduce a "seconds queued" fault bound; the only cap is memory.
- **Tool calls raised while the model reads `TALKER_BACKGROUND_EVENTS`** carry
  `background: true`. The server refuses side-effecting tools with a spoken
  reason and runs read-only ones (`readOnlyTools` in `control.go`). Never make
  this fail-closed by ending the session — the model does this routinely.
- **Notifications are acked only after being heard**: uninterrupted audio,
  playback drained, `turnComplete`. Sending ≠ delivered. Barge-in leaves them
  in the inbox.
- **Voice activity detection is Gemini's, server-side, on a continuous stream.**
  The browser never gates audio on volume: server VAD needs the silence for its
  noise floor and prefix padding, and barge-in is its decision — a client gate
  would false-trigger on Talker's own speaker output. The local RMS check in
  `mic-worklet.js` only informs the quiet gate. Tune phantom turns with
  `-speech-start`/`-speech-prefix` (setup.go), never with a client threshold.
  Mute is the one sanctioned pause and sends `audioStreamEnd`.
- **Every Hermes turn Talker starts is a durable run**, including `ask_hermes`,
  which waits briefly and then leaves the run to the ordinary monitor. A
  synchronous `/api/sessions/{id}/chat` outlives the tool-call timeout and
  leaves an untracked, unapprovable turn writing into the conversation.
- **Talker follows sessions by their messages, not runs.** Turns added from the
  Hermes web UI, CLI or chat apps create no run Talker can see. A follow polls
  the session row and announces each new assistant row with
  `finish_reason:"stop"`, skipping turns whose user message matches a Talker
  task's prompt (that task announces itself). Approvals raised in other clients
  stay in those clients.
- Same-origin checks honour `X-Forwarded-Host` (`internal/origin`). A Host-must-
  be-localhost check was deliberately removed; do not re-add it.

## Gotchas

- **ADK `functiontool` validates returned values** against a schema inferred
  from the Go return type. `json.RawMessage` fields fail validation at runtime
  *after* the side effect ran. Return the JSON-native `taskView`, never
  `tasks.Task`.
- Hermes: use the API Server adapter (`:8642`, bearer `API_SERVER_KEY`), not the
  dashboard (`:9119`). Runs are persisted locally *before* `POST /v1/runs` with
  a stable `Idempotency-Key`; interrupted runs are never auto-restarted. Run ID
  and session ID are different things, except that API-started runs appear in
  `/api/sessions` under their run ID.
- Hermes gotchas found against v0.21.4: session titles must be unique (let it
  auto-title); `/v1/skills` returns 500; list endpoints use `{object:"list",
  data:[…]}`; `/api/sessions/{id}/messages` returns the *oldest* rows whenever
  `limit` is sent without `order=latest`, and its `session_id` names the live
  continuation when compression rotates the session. Message `id`s are global
  insertion order, so they keep increasing across that rotation. A run's
  session row exists only once its turn starts; until then the session 404s.
- In-place compaction (Hermes's default) keeps the session ID but re-inserts
  the carried tail under fresh message IDs with the original timestamps, after
  a `display_kind:"hidden"` handoff row with empty content. A follow skips
  hidden rows, treats rows older than the newest it has seen as copies, and
  remembers recent answer digests.
- Hermes does not order overlapping turns of one session; their transcript
  writes interleave. `tasks.Manager` admits one turn per session: an explicit
  `session_id` is refused, and a conversation request goes to a separate
  session, while a Talker task writes there (resolved through rotation: a run
  keeps reporting the session it was admitted to) or the newest user message
  from another client is under ten minutes old and unanswered.
- Startup calls `/v1/capabilities` and refuses to run on 401. Before this,
  a wrong key produced a cheerful `hermes=true` and every tool silently failed.
- The system prompt intentionally keeps replies short; the model will decline
  to read 400 words even when asked. Use direct worklet injection to test long
  playback, not prompting.
- `speechConfig.languageCode` is accepted by `gemini-3.8-live` and pinned to
  `en-US` because the model drifts languages at temperature 1.1.

## Working here

- `go fmt ./...` (there is no `gofmt` on PATH). `go vet ./... && go test -race ./...`.
- Frontend: `node --test internal/web/*.test.js`. No `package.json` and no
  frontend dependencies by design; worklets are plain ES modules under
  `internal/web/assets/` and must be listed in `web.go`'s allowlist.
- `go run . -check` exercises the real constrained setup against Gemini and
  is the fastest way to validate a setup change (costs a few cents).
- Real-browser smoke uses Playwright installed *outside* the repo in
  `/tmp/kilo/talker-browser`. Keep it there.
- API keys: `GEMINI_API_KEY_FILE` exists so keys never appear in argv or
  shell history. Never print, log or commit them; redact `access_token=` in
  any captured URLs.
