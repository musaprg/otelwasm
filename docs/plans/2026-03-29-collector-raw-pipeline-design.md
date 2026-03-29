# Collector Raw Pipeline Design

**Status:** Draft

**Goal:** Explore an end-to-end OpenTelemetry Collector architecture that keeps telemetry in OTLP protobuf form across the pipeline so receivers, processors, exporters, and wasm plugins can operate without converting payloads into `pdata`.

**Audience:** Maintainers considering a strategic architecture change beyond `wasmplugin` optimization.

**Related Documents:**
- [`2026-03-29-wasmplugin-raw-bytes-mode-design.md`](./2026-03-29-wasmplugin-raw-bytes-mode-design.md)
- [`2026-03-29-otlp-wire-patch-engine-design.md`](./2026-03-29-otlp-wire-patch-engine-design.md)

## 1. Problem Statement

Approach 1 removes redundant host/guest boundary conversions. Approach 2 makes guest-side editing possible without `pdata`. Neither design removes the core fact that the Collector pipeline itself is still defined in terms of `pdata`.

As long as receivers emit `pdata`, processors consume `pdata`, and exporters accept `pdata`, the host must keep unmarshaling and rematerializing telemetry at pipeline boundaries. That limits the upside of wasm optimization.

This document describes the larger, more disruptive option: a raw OTLP pipeline representation for the Collector itself.

## 2. Scope

### In Scope

- Define what a raw payload representation would look like inside the Collector.
- Describe receiver, processor, exporter, and fan-out behavior under a raw model.
- Identify migration and compatibility strategies.

### Out of Scope

- Implementing this architecture now.
- Maintaining full source compatibility with existing Collector component APIs.
- Solving every optimization for every encoding and transport.

## 3. Design Summary

Introduce a new internal telemetry representation:

```go
type RawTelemetry struct {
	Signal   SignalType
	Encoding EncodingType
	Payload  []byte
	Meta     TelemetryMeta
}
```

Where:

- `SignalType` is traces, metrics, or logs.
- `EncodingType` is initially OTLP protobuf only.
- `Payload` contains the canonical OTLP wire bytes.
- `Meta` contains routing and envelope metadata that should not require full payload decode.

The pipeline becomes:

1. Receiver produces `RawTelemetry`.
2. Processor transforms `RawTelemetry`.
3. Exporter consumes `RawTelemetry`.

`pdata` becomes a compatibility layer instead of the primary representation.

## 4. Why This Is Hard

The current Collector architecture is deeply coupled to `pdata`:

- public component interfaces are typed around `pdata`
- mutability semantics rely on `pdata`
- fan-out and cloning assumptions rely on `pdata`
- many contrib components expect rich structured access

Moving to a raw pipeline is not a local optimization. It is a broad API and ecosystem change.

## 5. Representation Choices

There are two realistic representations.

### Option A: Pure Raw Bytes

Each stage receives only OTLP wire bytes.

Pros:
- simplest contract
- minimum representation overhead

Cons:
- routing and metadata access become expensive unless duplicated elsewhere
- every component must understand raw parsing

### Option B: Raw Bytes Plus Side Metadata

Each stage receives OTLP wire bytes plus a small metadata envelope with pre-extracted fields.

Pros:
- avoids decoding the whole payload for routing, sampling, and observability
- better ergonomics for pipeline infrastructure

Cons:
- metadata extraction cost still exists
- metadata can drift from payload if mutation rules are sloppy

Recommendation: if this path is ever pursued, use raw bytes plus side metadata.

## 6. Receiver Design

Receivers should emit canonical OTLP protobuf bytes as early as possible.

Examples:

- OTLP receivers can often preserve incoming protobuf bytes with minimal work.
- Non-OTLP receivers must still encode into OTLP protobuf once.

This means some pipelines can become nearly decode-free, while others still pay an initial encode cost at ingress.

## 7. Processor Design

Processors split into three categories.

### Raw-Capable Processors

These can use a wire patch engine and operate directly on bytes.

Examples:

- attribute insertion
- filtering
- renaming
- resource enrichment

### Metadata-Only Processors

These operate on side metadata and may not need to touch payload bytes.

Examples:

- routing
- throttling decisions
- certain sampling policies

### Structured Processors

These still require a structured representation.

Examples:

- complex aggregations
- processors with heavy semantic inspection
- existing components not yet ported

Structured processors would force a decode bridge from raw to `pdata` unless rewritten.

## 8. Exporter Design

Exporters also split into categories.

### Native OTLP Exporters

Can often forward protobuf bytes directly or with minimal wrapping.

### Non-OTLP Exporters

Need conversion from OTLP protobuf to their target representation.

The raw pipeline helps most when the source, internal transforms, and destination all align around OTLP protobuf.

## 9. Fan-Out, Mutation, and Ownership

The current Collector relies on shared mutable `pdata` rules. A raw pipeline should instead use explicit ownership:

- `Payload` is immutable by convention.
- Processor mutations produce a new payload.
- Unchanged payloads may be reference-counted or shared by slice aliasing.

This model is cleaner for concurrency, but it changes assumptions across the pipeline.

## 10. Compatibility Strategy

A hard cutover is unrealistic. The only plausible path is a layered migration:

### Layer 1

Add optional raw interfaces alongside existing `pdata` interfaces.

### Layer 2

Port selected components with high throughput and simple transformations.

### Layer 3

Add bridges:

- `pdata -> raw`
- `raw -> pdata`

### Layer 4

Gradually move high-volume deployments toward mostly-raw pipelines.

This implies a long coexistence period.

## 11. Relationship to Other Designs

[`2026-03-29-wasmplugin-raw-bytes-mode-design.md`](./2026-03-29-wasmplugin-raw-bytes-mode-design.md) is a local optimization inside the existing architecture. It is useful even if the Collector never adopts a raw internal representation.

[`2026-03-29-otlp-wire-patch-engine-design.md`](./2026-03-29-otlp-wire-patch-engine-design.md) is a foundational library for any raw processor implementation, whether inside wasm or outside it. A collector-wide raw pipeline without a robust editing engine would still leave processors unable to perform common transforms ergonomically.

## 12. Risks

### Risk: Ecosystem breakage

Most existing Collector components would not work natively with the new representation.

### Risk: Operational complexity

Mixed raw and structured pipelines create bridges, capability detection, and additional testing burden.

### Risk: Limited payoff for non-OTLP workloads

If many receivers or exporters are not OTLP-native, the raw model still requires encode or decode bridges.

### Risk: Metadata duplication

Side metadata may become inconsistent with payload bytes after transformation.

Mitigation:
- define metadata ownership rules clearly
- require processors that mutate relevant fields to update metadata or invalidate it

## 13. Testing Strategy

- golden interoperability tests across raw-only and bridged pipelines
- conformance tests for fan-out and mutation semantics
- benchmark suites for:
  - OTLP in -> raw processors -> OTLP out
  - non-OTLP ingress -> raw pipeline
  - mixed raw and `pdata` bridges

## 14. Decision Criteria

This design should only move forward if all of the following are true:

- benchmarks show boundary-only optimization is insufficient
- target deployments are predominantly OTLP protobuf end to end
- maintainers are willing to support a long migration period
- enough high-volume processors can actually benefit from raw editing

## 15. Recommendation

Do not start here.

Start with the smaller designs first:

1. transport raw bytes across wasm using [`2026-03-29-wasmplugin-raw-bytes-mode-design.md`](./2026-03-29-wasmplugin-raw-bytes-mode-design.md)
2. make raw editing practical using [`2026-03-29-otlp-wire-patch-engine-design.md`](./2026-03-29-otlp-wire-patch-engine-design.md)

Only revisit a collector-wide raw pipeline if those steps demonstrate compelling wins and reveal that host-side `pdata` conversions are still the dominant bottleneck.
