# Maintained Kiro plugin

This local fork keeps CLIProxyAPI's `kiro` provider identity and upstream MIT license. It does not create another credential cache, refresh loop, or agent harness. Existing Kiro accounts, model IDs, and client conversations remain usable.

Upstream base: `xiaokui-dev/cliproxyapi-kiro-plugin` at `f7b60a605c3613c18e4011b922f93b5bad70522c`.

## Current changes

- Forward declared effort, adaptive thinking, display preference, and output limits to Kiro.
- Advertise the verified effort levels for exact supported Kiro model IDs.
- Reject unsupported controls before inference instead of silently discarding them.
- Expose read-only per-account plan credits through the standard quota RPC on compatible CLIProxy v8 hosts.
- Preserve dotted Kiro model IDs. Hyphenated Claude account routes are distinct.

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
tool names/arguments, but are not provider token counters. Hidden reasoning,
cache usage and other provider overhead are not observable. Monthly credit
balances come directly from Kiro; do not convert these estimates into credits.

CLIProxy may strip these headers unless response-header forwarding is enabled.
The T3 integration labels token estimates explicitly without relying on forwarded
headers. This release does not change the host response-header policy.
