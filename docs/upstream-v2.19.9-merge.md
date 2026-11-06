# Upstream v2.19.9 Merge Candidate

## Scope

- Local base: `1dea1bdc29d5a4377c719a6e686478656f02ee24`.
- Upstream: `ranxi2001/sub2api`, branch `production`.
- Upstream commit: `bde8f910f574ce7e1e5893a9bdcac1c7c455bc49` (v2.19.9).
- Candidate branch: `integration/ranxi-production-20261106`.
- The candidate contains all 57 upstream commits missing from the local base.

## Resolution

The two conflicting files were `openai_gateway_passthrough.go` (five blocks)
and `openai_gateway_response_handling.go` (six blocks).

Both parsers retain the configured retry attempt buffer, opaque preamble
progress, size bounds, output commit guards, and the existing retry classifier.
Upstream SSE flush observation and trusted plugin event delivery are retained.
API key pool retries and OAuth plugin delivery keep their existing account-type
boundaries; no new setting or broader error classification was introduced.

The passthrough heartbeat path also needed a semantic adaptation outside the
textual conflicts. Receiving semantic content cannot stop the keepalive ticker
or suppress heartbeats while that content remains private in a retry buffer.
The existing heartbeat regression caught this interaction during integration.

`TestPluginSSEPoolRetriesRemainPrivate` additionally exercises both parsers with
the plugin setting enabled and trusted provenance. Reasoning, tool calls and
text stay private on BPS failure, transport failure or truncation. Successful
attempts first flush at the successful terminal, with matching delivery timing.

## Validation

Validation used the Windows amd64 Go 1.27.3 toolchain and pnpm 9.15.9.

- Selected service regressions and tests from changed service files: PASS,
  27.731 seconds. Includes plugin delivery, timing, configured retry buffering,
  heartbeat safety, cancellation, output bounds, capacity and policy handling,
  transport errors, model synchronization, and parsing fast paths.
- Handler retry and output-limit regressions: PASS, 212.505 seconds. Includes
  Responses, Messages and Chat, retry count exhaustion, fallback account order,
  unchanged request bodies/cache keys/session headers, and single usage settlement.
- Request timing, configuration, account timer and account timer API packages: PASS.
- Frontend affected suites and local monitor/embedded URL regressions: 22 files,
  630 tests passed.
- Locale completeness: three tests passed; `vue-tsc -b` and Vite production build passed.
- Backend production build with embedded frontend and CGO disabled: PASS.
- Conflict markers, unresolved index entries and whitespace checks: clear.

No production request was sent and no database migration was executed.
PostgreSQL integration tests and race-detector runs were outside this validation.
Upstream migration `335_account_daily_schedulable_timers.sql` remains included.

## Follow-up Review

A second review on November 7, 2026 added
`openai_merge_retry_invariants_test.go`; production code was unchanged.
The review directly covers both host parsers while private text, reasoning,
tool calls, encrypted reasoning/compaction, or an open event are buffered.
Multiple heartbeats remain comments and do not create a data-event flush timing.
Downstream heartbeat write and flush failures remain visible to the handler
even without context cancellation, while upstream terminal and cached-token
usage are drained without leaking private output or marking client completion.

Expanded streaming, flush, disconnect, plugin, timeout and classification
regressions passed in 24.188 seconds (669 passing test/subtest events).
The two new merged-retry invariant tests also passed three consecutive runs
in 1.811 seconds using controlled time.

The complete service unit package was also attempted with a 360-second timeout,
excluding the previously known Windows test
`TestRuntimeProxyChainRealTwoRefusalsOnlySendOnePOST`. It did not pass: it recorded
22,197 passing test/subtest events and 29 failing events, including parent tests.
This result must not be described as a green full-suite run.

The persistent failures were reproduced separately on the unchanged local base:

- `TestMistralReviewSDKAccumulation`: Windows `python3` application alias exits
  with status 9009; the pinned Python SDK accumulator was not executed.
- `TestRuntimeProxyFallbackRealRefusedConnection`: Windows `connectex` refusal
  is not recognized by the existing proxy fallback policy.
- `TestOpenAIWSHTTPBridgeSessionIsolationAcrossSameSessionHash`: the client
  waits for an event until the fixture's ten-second context expires.
- The quality-plan and Ollama callback tests, regional authentication and
  scheduler tests, and safety prompt permissions test also fail on the base.

`TestIssue578ExplicitRefreshWinsDelayedHeader` failed in the full-package run
but passed with count=1 on both candidate and base. With count=3, both reproduce
the same two later-iteration failures (expected quota 100, actual 95), consistent
with test state surviving between iterations. The bounded-memory test passed
independently on both. These comparisons establish that the
observed persistent failures are not introduced by this merge; they do not
resolve those existing failures or replace PostgreSQL and race-detector CI.

## Applying

The original `production` checkout is unchanged. If it still points to the local
base above, the reviewed candidate can be applied with:

```sh
git merge --ff-only integration/ranxi-production-20261106
```

The candidate is local; it has not been pushed or deployed.
