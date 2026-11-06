# Maintained Kiro plugin

This local fork keeps CLIProxyAPI's `kiro` provider identity and upstream MIT license. It does not create another credential cache, refresh loop, or agent harness. Existing Kiro accounts, model IDs, and client conversations remain usable.

Upstream base: `xiaokui-dev/cliproxyapi-kiro-plugin` at `f7b60a605c3613c18e4011b922f93b5bad70522c`.

## Current changes

- 0.4.0-local.2: clamp client `max_tokens` into Kiro's 1024–64000 range instead of rejecting the request (Claude Code sends values outside it).

- Stream AWS frames incrementally through host callbacks, including text, reasoning and tool arguments.
- Forward declared effort, adaptive thinking, display preference, and output limits to Kiro.
- Advertise the verified effort levels for exact supported Kiro model IDs.
- Reject unsupported controls before inference instead of silently discarding them.
- Expose read-only per-account plan credits through the standard quota RPC on compatible CLIProxy v8 hosts.
- Preserve dotted Kiro model IDs. Hyphenated Claude account routes are distinct.
- 0.4.0-local.1: use Kiro usage events (provider tokens, else context percentage x discovered context window) for reported usage; map context overflow to HTTP 400 "prompt is too long" so clients can compact; classify quota/capacity/blocked errors; estimate `count_tokens` locally (o200k BPE); keep images returned by tools.

## Compatibility contract

- Keep provider ID `kiro`; do not rename existing account files or rewrite transcripts.
- Configure each client's model capabilities separately when its protocol cannot discover them.
- Treat model catalogue schemas as the authority for supported controls. Verify changed schemas before expanding the allowlist.
- Distinguish provider-reported tokens, estimated tokens, API-equivalent dollars, and account credits. Never infer account credit balances from model aliases or token estimates.
- Do not automatically replay a partially delivered response or executed tool call.
- Keep request bodies, credentials, live transcripts, and account-specific evidence out of this repository and release artifacts.

## Releases and updates

Run `python3 scripts/build_release.py`. It runs tests, race checks, and vet before producing a versioned library and SHA-256 manifest in ignored `dist/`.

Install a versioned artifact (`kiro-v<VERSION>.dylib` on macOS), retaining an immutable backup outside the host's plugin discovery directory. Set CLIProxy's `plugins.configs.kiro.store.version` to that exact local version, and mark its store source as local-managed rather than the upstream release repository. The host may delete unselected plugin files, so its discovery directory is not a rollback store.

The version pin controls discovery; it cannot prevent a user explicitly installing another plugin version. Before accepting CLIProxy, Claude CLI, or T3 updates, verify plugin registration, catalogue, effort forwarding, tool round trips, cancellation, account quota, and retained model IDs. Pinning is controlled rollout, not a guarantee against incompatible host updates.

Merge upstream changes into a review branch; do not update the running plugin automatically. Do not publish release tags until offline checks and isolated live compatibility tests pass. Maintain the last known-good artifact and manifest for rollback.

## Remaining usage work

The upstream plugin currently estimates message token counts and exposes credits through its custom management endpoint. Standard quota capability is implemented. Native metering events, precise token counters, account attribution in historical sessions, and consumer-side display must be verified independently. A model-price alias can affect dollar estimates, but cannot manufacture token counts or account credit totals.

Source fork: https://github.com/AryanRG/cliproxyapi-kiro-plugin. Local releases are tested artifacts; public release assets, when available, are published separately.

Token accounting: Plugin executor responses include `X-Kiro-Token-Usage: estimated` and
`X-Kiro-Token-Estimator: utf8-bytes-div4`. Counts include returned text and
tool names/arguments and streamed reasoning, but are not provider token counters.
Hidden reasoning, cache usage and other provider overhead are not observable. Monthly credit
balances come directly from Kiro; do not convert these estimates into credits.

CLIProxy may strip these headers unless response-header forwarding is enabled.
The T3 integration labels token estimates explicitly without relying on forwarded
headers. This release does not change the host response-header policy.

## Incremental streaming

Streaming requires host HTTP operations, HTTP stream read/close, and asynchronous
executor emit/close callbacks. CLIProxyAPI v8.0.49 was exercised live with Messages
and Responses. Older hosts lacking these callbacks fail clearly before inference;
there is no buffered fallback or credential-refresh owner in the plugin.

The plugin returns immediately, emits a message-start event, and consumes bounded
AWS frames with prelude/message CRC validation. Text and reasoning deltas, restored
tool names, fragmented arguments, parallel tool IDs, and terminal usage are emitted
in order. Inline bracket-style tool calls remain supported and are deduplicated
against structured calls. Invalid arguments or incomplete/error frames fail rather
than manufacturing an empty tool input or successful completion.

Each streaming request is issued once. Disconnect, quiesce, shutdown, a six-minute
upstream idle interval, a 30-minute absolute deadline, or a 64 MiB upstream body
limit cancels the host HTTP operation and closes both stream handles. Pings every
ten seconds do not count as upstream activity and do not extend either deadline.
The non-streaming compatibility path retains its existing empty-response behavior.

The host Responses translator ignores Claude ping events. Clients requiring
transport keepalives must provide their own bounded SSE heartbeat handling. Our
Codex adapter relays real streams and emits status-only progress events during gaps;
its 360-second upstream read timeout remains independent of those heartbeats.
No tool call, assistant text, or completion is invented by a heartbeat.

## Usage events and context overflow

Kiro usage events are parsed when present: `metadataEvent`/`messageMetadataEvent`
`tokenUsage` (provider counts: uncached input, output, cache read/write),
`contextUsageEvent.contextUsagePercentage`, and `meteringEvent` credits. Input
tokens prefer provider counts, then percentage x model context window (catalogue
`maxInputTokens`, else known 1M models, else 200k), then the bytes/4 estimate.
Final counts are sent in the Anthropic `message_delta` usage and the non-stream
response. Non-stream `X-Kiro-Token-Usage` is `provider`, `context-percentage` or
`estimated`; streaming headers are fixed before any Kiro event, so they always
say `estimated` and the `message_delta` usage is authoritative. Credits appear
only as `X-Kiro-Credits`/`X-Kiro-Credit-Unit` on non-stream responses, never as
tokens. The live event shapes were taken from reference implementations and have
not been re-probed against Kiro in this fork.

`message_start` is deferred until the first content, reasoning, tool or usage
event; pings start only after it. An upstream error before that point closes the
stream with a structured HTTP error; after it, an Anthropic `error` SSE event is
emitted first. `CONTENT_LENGTH_EXCEEDS_THRESHOLD`, "Input is too long", or an
empty turn at contextUsagePercentage >= 100 becomes HTTP 400
`invalid_request_error` "prompt is too long: ...". `MONTHLY_REQUEST_COUNT` is 429
with Retry-After to the next UTC month, capacity/rate reasons are retryable
503/429, and the blocked-account 403 is labelled; token-expiry 403 stays 403.

## Count tokens

`executor.count_tokens` returns `{"input_tokens":N}` computed locally with the
o200k_base BPE (pure Go, vocabulary compiled in, works offline) plus
Anthropic's documented image estimate. Kiro has no count endpoint, and the
Anthropic tokenizer differs, so this is an estimate (`X-Kiro-Token-Usage:
estimated`, `X-Kiro-Token-Estimator: o200k-bpe`), not a provider count.
