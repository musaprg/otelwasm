# OTLP Wire Patch Engine Design

**Status:** Draft

**Goal:** Provide a guest-side library that can inspect and modify OTLP protobuf payloads directly at the wire level, without converting them into `pdata` or generated Go protobuf structs.

**Audience:** Maintainers of the guest SDK and authors of wasm-only processors that need high-performance payload transformations.

**Related Documents:**
- [`2026-03-29-wasmplugin-raw-bytes-mode-design.md`](./2026-03-29-wasmplugin-raw-bytes-mode-design.md)
- [`2026-03-29-collector-raw-pipeline-design.md`](./2026-03-29-collector-raw-pipeline-design.md)

## 1. Problem Statement

Switching `wasmplugin` to raw payload transport removes redundant boundary conversions, but it does not answer the harder question: how should a guest plugin actually edit OTLP payloads without reconstructing the full message tree?

Generic protobuf tooling is not sufficient for this use case because:

- OTLP payloads are deeply nested.
- Most guest transforms need schema-aware navigation.
- Purely generic wire walkers are too low-level for practical plugin authoring.
- Full unmarshaling defeats the purpose.

The missing piece is an OTLP-specific wire patch engine that parses protobuf bytes into a lightweight cursor and index structure, edits only targeted subtrees, and rebuilds only the changed regions.

## 2. Scope

### In Scope

- Parsing OTLP traces, metrics, and logs protobuf wire format.
- Schema-aware traversal using OTLP field numbers and message boundaries.
- Selective subtree rewrite for common transform classes.
- Guest-side APIs for reading and mutating payloads without `pdata`.

### Out of Scope

- A fully generic protobuf editor for arbitrary schemas.
- Perfect in-place mutation of arbitrary payloads.
- Support for non-OTLP encodings such as JSON or Arrow.

## 3. Core Insight

Direct byte mutation is only easy when the new encoded value has the same encoded length as the old value. Most useful telemetry edits do not satisfy that constraint.

Examples:

- Adding an attribute changes the size of the repeated `attributes` field.
- Replacing a string with a longer value changes the field length varint.
- Removing a span or log record changes the length of parent repeated fields.

Therefore, the practical design is not "blind in-place mutation". The practical design is:

1. Parse the wire format into offsets and field metadata.
2. Leave untouched slices referencing the original input buffer.
3. Re-encode only changed leaf or subtree segments.
4. Rebuild ancestor length-delimited envelopes up to the root.

This is still "no deserialize into Go structs", but it is not "no parse".

## 4. Design Summary

Implement a guest library that exposes three layers:

### Layer 1: Low-Level Wire Reader

- Reads protobuf tags, wire types, varints, fixed-width values, and length-delimited slices.
- Records absolute byte ranges.
- Never allocates full message objects.

### Layer 2: OTLP Schema Cursor

- Knows OTLP traces, metrics, and logs field numbers.
- Navigates payloads as typed cursors such as `TracesCursor`, `SpanCursor`, `MetricCursor`, `LogRecordCursor`.
- Exposes direct access to relevant fields and child iterators.

### Layer 3: Patch Builder

- Accepts edit operations such as add attribute, replace attribute, drop span, rewrite body.
- Rebuilds only modified branches.
- Produces a final root byte slice.

## 5. Representation

The library should represent messages as spans over the original byte slice plus a small amount of indexing metadata:

```go
type MessageRef struct {
	Input  []byte
	Start  int
	End    int
	Fields []FieldRef
}

type FieldRef struct {
	Number      int
	WireType    int
	TagStart    int
	ValueStart  int
	ValueEnd    int
	DecodedLen  int
}
```

Higher-level OTLP cursors wrap these references:

```go
type SpanCursor struct {
	msg MessageRef
}
```

This gives traversal without materializing the message into nested Go structs.

## 6. Edit Model

The edit model should be append-only from the perspective of the original input:

- Original byte ranges are immutable.
- Patch operations describe replacements for specific field ranges or appended repeated elements.
- Final output is assembled from original slices plus new encoded fragments.

Example operations:

- `SetSpanName(path, "new-name")`
- `UpsertSpanAttribute(path, key, value)`
- `DropSpan(path)`
- `RewriteLogBody(path, newValue)`

The engine should internally normalize these into subtree rewrites rather than scattered byte pokes.

## 7. Supported Mutation Classes

### Good Fit

- Add, remove, or replace attributes.
- Rename span or metric names.
- Filter spans, metrics, datapoints, or log records.
- Rewrite selected scalar fields.
- Attach derived attributes or resource metadata.

### Harder But Possible

- Reordering repeated fields.
- Deep nested updates in `AnyValue`.
- Large-scale fan-out or fan-in rewrites.

### Poor Fit

- Heavy analytics requiring arbitrary random access to many cross-linked fields.
- Transforms that benefit from a fully materialized object graph.

## 8. OTLP-Specific Code Generation

Do not hand-maintain all field numbers and traversal rules.

Generate cursor code from OTLP proto definitions or from a compact schema manifest:

- Traces message hierarchy
- Metrics message hierarchy
- Logs message hierarchy
- `AnyValue`, `KeyValue`, `InstrumentationScope`, `Resource`

Generated code should provide:

- field constants
- child iterators
- typed accessors
- patch helpers for common OTLP nodes

This is the key reason this design should be OTLP-specific, not generic protobuf infrastructure.

## 9. API Sketch

```go
type TracesDocument struct {
	root MessageRef
}

func ParseTraces(input []byte) (TracesDocument, error)

func (d TracesDocument) ResourceSpans() ResourceSpansIterator

func (s SpanCursor) Name() []byte
func (s SpanCursor) Attributes() AttributeIterator
func (s SpanCursor) UpsertStringAttribute(key, value string)
func (s SpanCursor) Drop()

func (d TracesDocument) Build() ([]byte, error)
```

The API should be biased toward mutation workflows, not toward exposing every wire detail.

## 10. Memory Model

The engine should minimize allocation by:

- Reusing the input byte slice for unchanged regions.
- Allocating patch fragments only for modified fields and enclosing messages.
- Building the final output in a single final buffer when possible.

This means unchanged payload portions are effectively zero-copy inside the guest, even though host-to-guest transfer still involves a memory copy.

## 11. Correctness Constraints

The engine must preserve:

- field ordering for untouched fields
- unknown fields
- duplicate fields if they already exist
- repeated field semantics
- oneof semantics
- canonical OTLP protobuf validity

Unknown fields are especially important. A generated Go struct round-trip sometimes normalizes representation. This engine should preserve untouched bytes exactly whenever possible.

## 12. Risks

### Risk: Complexity is higher than expected

Wire-level editing for nested length-delimited messages is subtle.

Mitigation:
- Start with traces only.
- Limit first release to a narrow mutation set.

### Risk: Plugin author ergonomics are still poor

Mitigation:
- Provide high-level mutation helpers for common OTLP tasks.
- Avoid exposing raw field numbers in end-user APIs.

### Risk: Bugs produce corrupted telemetry payloads

Mitigation:
- Add differential tests against `pdata`.
- Validate final payloads with standard OTLP unmarshal in tests.

## 13. Testing Strategy

### Differential Correctness Tests

For each supported operation:

1. Apply the transform using `pdata`.
2. Apply the same transform using the wire patch engine.
3. Unmarshal both outputs with standard OTLP tooling.
4. Assert semantic equivalence.

### Preservation Tests

- Unknown field retention.
- Untouched subtree byte identity.
- Duplicate attribute and repeated field behavior.

### Fuzzing

- Fuzz malformed payloads.
- Fuzz partial truncation.
- Fuzz large repeated structures.

### Benchmarks

- no-op traversal
- add attribute
- rename span
- drop span
- drop log record

Benchmark against guest-side `pdata` unmarshal plus marshal.

## 14. Rollout Plan

### Phase 1

- Build low-level wire reader.
- Support traces parsing and typed iteration.

### Phase 2

- Implement patch builder for traces.
- Ship one high-value mutation such as attribute upsert.

### Phase 3

- Extend to logs and metrics.
- Add generated schema helpers.

### Phase 4

- Add higher-level authoring helpers for guest plugins.

## 15. Dependency and Relationship to Other Designs

This design depends on [`2026-03-29-wasmplugin-raw-bytes-mode-design.md`](./2026-03-29-wasmplugin-raw-bytes-mode-design.md) for efficient transport across the wasm boundary. Without raw transport, the boundary still pays marshal and unmarshal costs that reduce the value of this engine.

This design is also a prerequisite for the more ambitious pipeline changes described in [`2026-03-29-collector-raw-pipeline-design.md`](./2026-03-29-collector-raw-pipeline-design.md), because any end-to-end raw architecture still needs a structured, schema-aware way to transform payloads.

## 16. Recommendation

Treat this as the enabling technology for high-performance wasm processors. It is more complex than simply changing the ABI, but it is the part that makes "edit protobuf directly" practical rather than theoretical.
