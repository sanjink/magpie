# Gateway live speed

The gateway exposes an in-process estimate of a Codex reply while it is
streaming, for a menu bar or notch HUD that wants more than a completed
call's average. It is deliberately a side observer: it does not translate a
second time and does not change the bytes sent to the client.

## Endpoint

`GET /v1/magpie/live?session=<id>&turn=<turn-id>`

`turn` is optional. A missing session, an unknown session, or a turn that
does not match returns a fixed-shape object with `active: false` and `done:
false`. The fields are:

`active`, `done`, `session`, `turn`, `model`, `phase`, `started_at`,
`first_at`, `updated_at`, `estimated_tokens`, `speed`, `window_ms`,
`speed_estimated`, `stale`.

`phase` is one of `waiting`, `reasoning`, `text` and `tool`. `first_at` is
null until the first visible content event. `speed` is zero until the
estimator has enough evidence.

The endpoint is held tighter than the other status APIs: a direct,
non-proxied loopback call alone is answered. A remote call, a call passed
through a proxy or tunnel on this computer, and a caller with a gateway key
all receive `403`. It stores no prompt, reply text or credential, and the
response is marked `Cache-Control: no-store`.

## Which requests are followed

Only the main turn of a Codex conversation is registered. The gateway filters
out titles, descriptions, composer predictions, auxiliary and memory calls,
search and vision stand-ins, Codex subagents and Claude Code subagents. A
request needs a session header and a streamed body.

Both output paths feed the same observer:

- `serveAgent` for translated and provider-routed replies;
- `codexUpstreamOn` for Codex's native
  `/backend-api/codex/responses` relay.

The registration is idempotent on the request context. A retry or a handler
boundary cannot create a second row for the same request.

## Estimate

The shared SSE timing parser reads the client-facing deltas into `text`,
`reasoning` and tool-argument increments. It never decodes a vendor's private
protocol and never stores the delta itself.

For each non-empty delta:

```text
estimated = floor(ASCII characters / 4) + non-ASCII characters
estimated = max(estimated, 1)
```

The published speed uses a one-second sliding window ending at the latest
event. A value is published only when the window spans at least 300ms and
contains at least 8 estimated tokens. The result is capped at 10,000 tok/s.

After 1.5 seconds without a new event, `stale` becomes true and `speed`
becomes zero. The HUD then falls back to its local per-call exact value or the
turn average. When a request ends, `done` becomes true; the row remains
queryable for 30 seconds. The hub retains at most 64 requests, evicting the
oldest first.

When more than one model call belongs to one turn, the newest active row is
returned. Between calls, the last row is `done` and `active: false`, so the
caller can show a turn average while tools run. A completed usage row remains
the exact source and outranks the live estimate.

## Verification

- `TestLiveSpeedWindowAndStale`
- `TestLiveSpeedNeedsWindowAndTokens`
- `TestLiveSessionTurnAndRetention`
- `TestLiveSSEDeltasFourProtocols`
- `TestLiveTrackFiltering`
- `TestLiveEndpointLoopbackAndShape`
- `TestLiveCodexUpstreamIntegration`
- `TestLiveTranslatedIntegration`

Implementation: [`internal/gateway/live.go`](../../internal/gateway/live.go),
the observer in [`internal/gateway/ttft.go`](../../internal/gateway/ttft.go),
and the endpoint registration in
[`internal/gateway/gateway.go`](../../internal/gateway/gateway.go).
