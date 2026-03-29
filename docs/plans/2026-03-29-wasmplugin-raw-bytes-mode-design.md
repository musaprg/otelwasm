# Wasmplugin Raw Bytes Mode Design

**Status:** Draft

**Goal:** Eliminate protobuf unmarshal and remarshal at the host/guest boundary by introducing a `wasmplugin` execution mode that exchanges OTLP payloads as raw protobuf bytes.

**Audience:** Maintainers of `wasmplugin`, `wasmprocessor`, `wasmreceiver`, `wasmexporter`, and guest plugin authors willing to target a raw-bytes API instead of `pdata`.

**Related Documents:**
- [`2026-03-29-otlp-wire-patch-engine-design.md`](./2026-03-29-otlp-wire-patch-engine-design.md)
- [`2026-03-29-collector-raw-pipeline-design.md`](./2026-03-29-collector-raw-pipeline-design.md)

## 1. Problem Statement

The current architecture serializes and deserializes OTLP payloads multiple times when data crosses the WebAssembly boundary:

1. Host `pdata` is marshaled into protobuf bytes.
2. Guest unmarshals protobuf bytes into `pdata`.
3. Guest marshals the modified `pdata` into protobuf bytes again.
4. Host unmarshals those bytes back into `pdata`.

This cost is paid even when the guest only needs a narrow mutation such as adding an attribute, renaming a field, or filtering records.

The first optimization step is to remove the guest-side `pdata` representation entirely and let `wasmplugin` hand raw protobuf bytes to the guest. This does not by itself make the Collector pipeline raw end-to-end, but it removes two full conversion steps on every wasm call.

## 2. Scope

### In Scope

- Add a raw-bytes execution mode to `wasmplugin`.
- Replace the guest `pdata` API with a raw payload API for wasm-targeted plugins.
- Allow processors and exporters to send and receive raw OTLP bytes through the wasm boundary.
- Preserve the existing non-raw host pipeline for compatibility with the rest of the Collector.

### Out of Scope

- Removing `pdata` from the Collector host pipeline.
- Designing a generic protobuf editing engine.
- Supporting both old `pdata` guest plugins and new raw guest plugins forever in the same API surface.

## 3. Non-Goals

- Zero-copy sharing of host memory with guest memory.
- Full in-place mutation of arbitrary protobuf payloads.
- Format support beyond OTLP protobuf.

## 4. Design Summary

Introduce a second wasm ABI centered on opaque OTLP byte payloads:

- Host side stores current input and result as `[]byte`.
- Built-in host functions expose raw current payload bytes to the guest.
- Guest exports raw processing functions that accept or pull byte payloads and return byte payloads.
- Host unmarshals only once, after the guest returns, so that the rest of the Collector can continue using `pdata`.

This produces the following conversion count:

1. Host `pdata` to protobuf bytes before wasm call.
2. Guest edits bytes without converting to `pdata`.
3. Host protobuf bytes back to `pdata` after wasm call.

Compared with the current path, the guest-side unmarshal and remarshal disappear.

## 5. Proposed ABI

### Host-to-Guest Functions

Add raw variants of the existing built-ins:

- `current_traces_raw(buf, buf_limit) -> size`
- `current_metrics_raw(buf, buf_limit) -> size`
- `current_logs_raw(buf, buf_limit) -> size`
- `set_result_traces_raw(ptr, size)`
- `set_result_metrics_raw(ptr, size)`
- `set_result_logs_raw(ptr, size)`

These functions are semantically identical to the current byte-copy behavior, except they never decode the payload into `pdata` inside the guest support library.

### Guest-Facing API

Replace `pdata` interfaces with raw payload interfaces such as:

```go
type RawTracesProcessor interface {
	ProcessTracesRaw(payload []byte) ([]byte, *Status)
}

type RawMetricsProcessor interface {
	ProcessMetricsRaw(payload []byte) ([]byte, *Status)
}

type RawLogsProcessor interface {
	ProcessLogsRaw(payload []byte) ([]byte, *Status)
}
```

For receivers and exporters:

- Receivers produce OTLP protobuf bytes directly.
- Exporters consume OTLP protobuf bytes directly.

The guest helper package becomes a transport API, not a `pdata` facade.

## 6. Data Flow

### Processor Path

1. Host receives `pdata`.
2. Host marshals `pdata` once into OTLP bytes.
3. Host writes bytes into guest memory through `current_*_raw`.
4. Guest reads bytes and processes them using a raw editing library.
5. Guest writes result bytes through `set_result_*_raw`.
6. Host reads result bytes and unmarshals them once back into `pdata`.
7. Host returns `pdata` to the next Collector component.

### Exporter Path

1. Host marshals `pdata` to OTLP bytes once.
2. Guest exporter consumes raw bytes.
3. Guest performs protocol-specific output without constructing `pdata`.

### Receiver Path

1. Guest receiver obtains external input.
2. Guest encodes OTLP protobuf bytes directly.
3. Host reads bytes from guest memory.
4. Host unmarshals once into `pdata` to enter the Collector pipeline.

## 7. Host-Side Changes

### `wasmplugin`

Add a raw payload stack alongside the existing `pdata` stack:

```go
type RawStack struct {
	CurrentTracesBytes  []byte
	CurrentMetricsBytes []byte
	CurrentLogsBytes    []byte
	ResultTracesBytes   []byte
	ResultMetricsBytes  []byte
	ResultLogsBytes     []byte
	StatusReason        string
	PluginConfigJSON    []byte
}
```

The host module implementation should:

- Write `Current*Bytes` directly to wasm memory.
- Read result bytes directly from wasm memory.
- Avoid `ProtoUnmarshaler` in built-in host functions.

### `wasmprocessor`, `wasmreceiver`, `wasmexporter`

Each wrapper gains a raw mode:

- Marshal input once before entering wasm.
- Invoke the raw guest function.
- Unmarshal once after return if the Collector side still needs `pdata`.

This can coexist with the current mode during migration, but the long-term intent is to let the raw mode become the primary API for wasm-only plugins.

## 8. Guest-Side Changes

Replace `guest/api` and `guest/imports` with raw equivalents:

- `CurrentTracesRaw() []byte`
- `CurrentMetricsRaw() []byte`
- `CurrentLogsRaw() []byte`
- `SetResultTracesRaw([]byte)`
- `SetResultMetricsRaw([]byte)`
- `SetResultLogsRaw([]byte)`

The guest layer should not import `ptrace`, `pmetric`, or `plog`.

Guest plugins that still need structured access must explicitly choose a library that parses raw protobuf bytes. The preferred path is the OTLP-specific wire patch engine described in [`2026-03-29-otlp-wire-patch-engine-design.md`](./2026-03-29-otlp-wire-patch-engine-design.md).

## 9. Compatibility Strategy

There are two realistic migration strategies.

### Option A: Hard Break

- Replace the current guest API with raw APIs.
- Require all wasm plugins to migrate.

Pros:
- Cleaner codebase.
- No long-term dual-mode maintenance.

Cons:
- Immediate breaking change.

### Option B: Transitional Dual Mode

- Keep old `pdata` exports temporarily.
- Add raw exports and new guest helpers.
- Deprecate old mode quickly.

Pros:
- Easier migration.

Cons:
- More host-module complexity.
- Benchmarking and testing matrix doubles during transition.

Recommendation: use transitional dual mode only as a short-lived migration step, then remove `pdata` guest mode.

## 10. Performance Expectations

Expected wins:

- Remove guest-side protobuf decode into `pdata`.
- Remove guest-side protobuf encode from `pdata`.
- Reduce guest heap pressure.
- Reduce wasm execution time for simple transforms.

Expected costs that remain:

- One host marshal before entering wasm.
- One host unmarshal after leaving wasm.
- Byte copies between host memory and guest memory.

This design improves the boundary cost materially, but it does not by itself achieve end-to-end zero-deserialize processing in the Collector.

## 11. Risks

### Risk: Guest authors lose ergonomic APIs

Raw bytes are harder to work with than `pdata`.

Mitigation:
- Provide an OTLP-specific editing library.
- Document common recipes.

### Risk: Result payload correctness becomes guest responsibility

Malformed protobuf can now be emitted by guest code.

Mitigation:
- Validate returned bytes on the host boundary.
- Add focused conformance tests for traces, metrics, and logs.

### Risk: Dual mode complicates maintenance

Mitigation:
- Set a removal milestone for old `pdata` guest support.

## 12. Testing Strategy

- ABI tests for new host functions.
- Round-trip tests for traces, metrics, and logs raw mode.
- Negative tests for malformed guest output.
- Benchmarks comparing:
  - current `pdata` mode
  - raw mode without mutation
  - raw mode with simple attribute mutation

## 13. Rollout Plan

### Phase 1

- Add raw stack and raw host functions.
- Add raw guest helper package.
- Keep old mode intact.

### Phase 2

- Port one processor example to raw mode.
- Benchmark raw mode against current mode.

### Phase 3

- Port receiver and exporter paths.
- Deprecate `pdata` guest API.

### Phase 4

- Remove old guest `pdata` API and related host functions.

## 14. Open Questions

- Should raw guest APIs pull input via host functions or receive `(ptr, size)` as wasm-exported function parameters?
- Should the host preserve input bytes for no-op processors and allow a guest to signal "unchanged" without copying?
- Should validation of guest output be strict by default or configurable?

## 15. Recommendation

Implement this design first. It is the lowest-risk path to remove the most expensive redundant boundary conversions, and it creates the transport layer required by the OTLP wire patch engine in [`2026-03-29-otlp-wire-patch-engine-design.md`](./2026-03-29-otlp-wire-patch-engine-design.md).
