---
title: "Draft: FSST encoding for log message pages"
description: "Explore FSST with a shared table per page and offsets for individual strings."
draft: true
---

# Draft: FSST encoding for log message pages

**Author:** @dimitarvdimitrov

**Date:** 10/2026

**Sponsor(s):** TBD

**Type:** Feature

**Status:** Draft

**Related issues/PRs:** TBD

---

## Background

Log message columns currently use plain, length-prefixed strings with Zstd compression.
To decode individual strings the reader decompresses the complete values block.

FSST uses a trained table of symbols to compress strings.
Each symbol represents up to eight bytes.
Multiple strings can share one table and remain independently decodable.

This proposal uses [axiomhq/fsst](https://github.com/axiomhq/fsst).

// TODO dimitarvdimitrov remove or mvoe
[//]: # (The [batch APIs]&#40;https://github.com/axiomhq/fsst/blob/127a94cb9aecfa9b173a30a79aaf907efd132242/table.go&#41; provide compressed bytes and offsets for independent strings.)

## Problem Statement

The current layout couples string access to whole-page decompression.
We want to explore a layout that removes per-string varints and preserves independent access to compressed strings.

## Goals

- Find a cheaper replacement for Zstd for log message columns.
- Remove the varint length prefix from each string.
- Permit selective string decompression in a later reader optimization.
- TODO: format migration & compatibility guarantees

## Non-Goals

Other columns retain their current encodings and compression.

## Proposals

### Proposal 0: Retain plain strings with Zstd

This approach preserves the current implementation.
It retains per-string varints and requires whole-page decompression.

### Proposal 1: Shared table with symbol offsets

FSST acts as a value encoding.
Message columns use `ENCODING_TYPE_FSST` with `COMPRESSION_TYPE_NONE`.

Each page contains one trained table.
The encoder compresses each non-NULL string independently with that table.
The page stores the resulting byte ranges in an offsets array next to the FSST table and the symbol sequences.
Symbol sequences contain codes that reference table entries, plus literal escapes for bytes without a symbol match.
The space savings come from codes that replace longer byte sequences in the table.

Conceptually, the page contains:

```text
<presence bitmap size>
<presence bitmap>
<serialized FSST table>
<symbol offsets: uint32[N+1]>
<symbol sequences>

N = number of non-NULL strings
```

Offsets use little-endian integers and refer to byte positions within the symbol sequences.
The first offset is zero.
The final offset equals the total size of the symbol sequences in bytes.

String `i` occupies:

```text
symbolSequences[offsets[i]:offsets[i+1]]
```

The presence bitmap retains its existing page header.
There are no per-string varints.

#### String boundaries and decoded lengths

The FSST table does not contain string boundaries.
It contains symbols and their byte lengths.
Offsets identify the symbol sequence for each string.

This is the equivalent of the string lengths when strings are encoded with `ENCODING_TYPE_PLAIN` and `COMPRESSION_TYPE_ZSTD`.
The decoder derives the decoded length from symbols and literal escapes within each range.

// TODO dimitarvdimitrov figure out how we can integrate the allocator into this design

#### Accepted tradeoffs

Fixed-width offsets cost four bytes per boundary (`uint32`).
Short strings can therefore require more boundary metadata than varint lengths.
The goal of this is to be able to randomly access a string without decoding all previous offsets (as a varint would need).

// TODO dimitarvdimitrov add some numbers for how different the size of pages is with varint vs our int32 + how big the offsets are and if we need to use varint

Each page also carries a symbol table.
Small pages can incur substantial overhead.
// TODO dimitarvdimitrov how big is this tradeoff? can we compare a tiny symbols table for a page with a single string value `1` and then for one with 10 values from `0` to `9` and compare what the size amplification is
Independent pages avoid shared dictionary dependencies but require repeated table training.

FSST can produce larger output than Zstd.
// TODO dimitarvdimitrov we need numbers. we have numbers in fsst-go
The experiment accepts that possibility and measures the tradeoff against decode speed.

### Proposal 2: Apply FSST to the existing length-prefixed stream

This approach fits the existing compression hooks.
It retains per-string varints and requires whole-page decompression.
This is less desired because it closes the door to random access decompression.

### Proposal 3: Compress concatenated strings with decoded offsets

This approach removes per-string varints and preserves decoded boundaries.
Individual strings still require whole-page decompression.
We prefer symbol offsets because they preserve random access decompression.

## Other Notes

### Preliminary Go benchmark results

The standalone `fsst-go` prototype at commit `a166238` contains measurements from 2026-10-15.
The host uses an Apple M1 Pro CPU with Go 1.27.1 and one benchmark worker.
Measurements use [FSST revision `392aa0d`](https://github.com/axiomhq/fsst/tree/392aa0d5eac7621f855b80df1fd6dc71f204a94f) and `github.com/klauspost/compress v1.20.1`.
The batch API link above identifies a separate FSST revision.

The results below use 2,048 synthetic service-log strings, each 40–1,023 bytes long.
FSST trains its table on 512 separate sample strings.
Each case has seven samples per operation, with at least 200 ms per sample.
Rates are medians in decimal MB/s, calculated from uncompressed bytes.
An independent repeat differed by up to 6.13% in median throughput across the full benchmark.

| Codec and layout | Encode (MB/s) | Decode (MB/s) | Stored ratio |
| --- | ---: | ---: | ---: |
| FSST, independent strings, unknown decoded lengths | 457.0 | 2,664.5 | 3.22× |
| Zstd `SpeedFastest`, blocks up to 64 KiB | 387.5 | 1,276.7 | 5.88× |
| Zstd `SpeedDefault`, blocks up to 64 KiB | 340.0 | 1,287.9 | 5.76× |
| Zstd `SpeedBestCompression`, blocks up to 64 KiB | 63.4 | 1,523.7 | 6.27× |

A larger stored ratio means less stored data.
FSST uses approximately 1.8× as many stored bytes as the fastest Zstd block case on this corpus.
Its decode throughput is approximately 2.1× higher.
The grouped Zstd cases approximate the current whole-page compression model.
Loki page framing and reader overhead remain unmeasured.

The FSST row uses `Table.Decode`, which requires no original string length.
A separate `DecodeIntoExact` case reaches 3,193.3 MB/s, but requires a known decoded length.
The proposed layout does not store that length.

These are steady-state rates with reusable codec instances and destination buffers.
The throughput timer excludes table training and construction.
Fresh training for each Loki output page remains outside these measurements.
Loki ingestion and selective-query performance remain unmeasured.

Stored ratios include serialized models and boundary metadata.
The benchmark charges FSST eight index bytes per string, plus a final four-byte offset.
This includes an original-length field that generic decode ignores.
The proposed layout uses four bytes per symbol offset and needs its own size measurements.

The Zstd blocks contain two-byte string lengths.
Their directory costs eight bytes per block, plus a final four-byte offset.
Zstd frame checksums remain enabled.
The FSST case adds no checksum.
These differences prevent direct comparison with the proposed serialized page format.

The prototype contains raw samples in `results/tuned.json` and `results/repeat.json`.
Its `results/report.html` includes the full comparison and observed sample ranges.

### Integration

`tableBuffer.Message` selects the new encoding.
A dedicated FSST encoder and decoder retain the existing plain encoding for other columns.

The encoder buffers strings until the page flush.
It trains one table, encodes the strings, and writes the table, offsets, and symbol sequences.
The buffer must own the string bytes until the flush.

`compressWriter.Reset` uses its existing pass-through path.
`MemPage.open` retains the FSST payload in encoded form.
The value decoder owns FSST decompression.

The reader initially decodes requested sequential batches.
Offsets also permit direct access to individual compressed strings.
Predicate-aware access remains future work.
The current reader still decodes skipped rows, so offsets alone do not provide that optimization.

The page descriptor contains `ValuesCount`, which identifies the number of offsets.
The value decoder constructor currently accepts only bytes.
The integration needs count propagation or a count within the payload.
This detail remains open.

Page size estimates must include buffered strings before the encoder flush.
Uncompressed size statistics must describe decoded data.
CRC32 and stored size statistics must include the table and offsets.

### Protobuf changes

The prototype adds `ENCODING_TYPE_FSST` to `EncodingType`.
The dedicated Makefile target regenerates `datasetmd.pb.go`:

```bash
make pkg/dataobj/internal/metadata/datasetmd/datasetmd.pb.go
```

The broad `make protos` target deletes all generated protobuf files before regeneration.
The dedicated target limits regeneration to the affected file.

No FSST compression enum is necessary for this approach.
The table and offsets reside within the page payload.
They need no additional protobuf fields.

### Validation and open questions

Validation covers:

- Exact round trips for empty strings, Unicode, and arbitrary bytes.
- NULL values and pages with no non-NULL values.
- Multiple pages and reused builders.
- Ordered appends and incremental merges.
- Invalid offsets, truncated tables, and corrupt compressed streams.
- Compression ratio, encode throughput, decode throughput, and allocations.
- Table overhead and repeated table training costs.
- Accuracy of existing object size estimates.

Further work must resolve:

- How the decoder receives the number of strings.
- How decoded buffers integrate with the existing memory allocator.
- Whether fixed-width offsets need a more compact representation.
- Whether later readers need stored decoded lengths for exact allocation.

### String boundaries and decoded lengths illustration

This is an illustration of how strings are stored
from [String boundaries and decoded lenghts](#string-boundaries-and-decoded-lengths)

```text
FSST table:
    A → "hello"
    B → "world"

Input rows:          ["hello", "", NULL, "world"]
Presence bitmap:     [1, 1, 0, 1]
Symbol offsets:      [0, 1, 1, 2]
Symbol sequences:    [A, B]
```

Equal adjacent offsets represent an empty string.
NULL rows have no entry in the values stream.

After decompression, the non-NULL values have this representation:

```text
Decoded offsets:  [0, 5, 5, 10]
Decoded bytes:    "helloworld"
```

This representation matches the offsets and bytes that `columnar.UTF8` uses.

The library provides `EncodeBatch` and `DecodeBatch`.
These APIs produce symbol offsets and reconstruct decoded offsets.
The integration must serialize the symbol offsets itself.
