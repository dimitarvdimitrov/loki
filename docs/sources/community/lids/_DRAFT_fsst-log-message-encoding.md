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

The optimized benchmarks use [the FSST fork at revision `ee30e95`](https://github.com/dimitarvdimitrov/fsst/commit/ee30e95db62e).
The fork starts from [axiomhq/fsst revision `392aa0d`](https://github.com/axiomhq/fsst/tree/392aa0d5eac7621f855b80df1fd6dc71f204a94f).
It adapts `DecodeBatch` to the existing checked eight-symbol kernel.

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

FSST measurements from 2026-11-05 use the optimized batch kernel at fork revision `ee30e95`.
Zstd measurements remain the earlier 2026-10-15 controls with `github.com/klauspost/compress v1.20.1`.
Both experiments use an Apple M1 Pro, Go 1.27.1, and one benchmark worker.
The Zstd controls do not represent a new simultaneous run against the fork.

The results below use 2,048 synthetic service-log strings, each 40–1,023 bytes long.
FSST trains its table on 512 separate sample strings.
Each case contains seven samples per operation, with at least 200 ms per sample.
Rates are medians in decimal MB/s, calculated from uncompressed bytes.
The independent FSST repeat differs by at most 1.54% in batch decode medians across all four corpora.
The earlier Zstd experiment reports up to 6.13% variation across its full comparison.

| Codec and layout | Encode (MB/s) | Decode (MB/s) | Stored ratio |
| --- | ---: | ---: | ---: |
| FSST fork, independent strings, optimized batch decode | 465.0 | 5,192.2 | 3.33× |
| Zstd `SpeedFastest`, blocks up to 64 KiB | 387.5 | 1,276.7 | 5.88× |
| Zstd `SpeedDefault`, blocks up to 64 KiB | 340.0 | 1,287.9 | 5.76× |
| Zstd `SpeedBestCompression`, blocks up to 64 KiB | 63.4 | 1,523.7 | 6.27× |

A larger stored ratio means less stored data.
FSST uses approximately 1.77× as many stored bytes as the fastest Zstd block case on this corpus.
Its measured decode throughput is approximately 4.07× higher.
The grouped Zstd cases approximate the current whole-page compression model.
Loki page framing and reader overhead remain unmeasured.

#### Optimized batch decode

The fresh upstream batch control reaches 2,781.5 MB/s on logs.
The fork reaches 5,192.2 MB/s, a 1.87× gain.
Its independent repeat reaches 5,119.3 MB/s.
Batch speedups across logs, URLs, prose, and random text range from 1.85× to 1.90×.

The fork calls the checked eight-symbol kernel with each compressed range and the remaining page capacity.
It reconstructs decoded offsets from actual output sizes without individual decoded lengths.
The scalar fallback preserves buffer growth and legacy error behavior.
A capacity shortage can cause duplicate work for the current row.

In the same primary fork run, generic per-string decode reaches 2,701.3 MB/s on logs.
Checked `DecodeIntoExact` reaches 3,224.4 MB/s with individual decoded lengths.
The optimized batch kernel needs only sufficient destination capacity.
The exact API limits each kernel call to that row's decoded length.
The batch API exposes more capacity, so more row tails can use wide stores.

#### Allocation and random access

Every timed reusable batch decode sample records zero allocations and zero allocated bytes.
The destination capacity equals the total decoded page size, with no extra padding.
The output offset buffer contains `N+1` reusable entries.
These counts exclude initial buffers, table construction, and input offset conversion.

The log page retains 819,418 decoded bytes and 16,392 bytes of decoded offsets on the 64-bit host.
Cold decode with nil destinations records 33 allocations and approximately 4.15 MB allocated per page in both implementations.
The fork reaches 1,998.2 MB/s in that cold-buffer test, versus 1,470.0 MB/s upstream.
Reusable destinations avoid those repeated allocations and copies.
Loki allocator integration remains unmeasured.

Compressed offsets still permit direct access to one selected string.
A singleton `DecodeBatch` call needs that compressed range and two relative offsets, without prior rows or an individual decoded length.
A sufficient reusable scratch buffer avoids allocations.
The random access benchmark uses the maximum row size of 1,023 bytes for all row decoders.

For logs, warm random-order access takes 93.2 ns per row with the fork, versus 154.2 ns with upstream batch decode.
Fork generic decode takes 150.4 ns per row, and exact decode takes 124.7 ns.
All random access samples record zero allocations.
The test excludes storage I/O and persistent output copies.
The current Loki reader still needs a separate change to skip decode for unselected rows.

#### Storage accounting and limits

Stored ratios include serialized models and boundary metadata.
The measured FSST batch directory contains `uint32[N+1]` compressed offsets and one `uint32` total decoded size.
It costs 8,200 bytes for 2,048 rows, versus 16,388 bytes for the earlier directory with individual decoded lengths.
The payload contains 236,999 bytes, and the serialized table contains 812 bytes.
Total stored size is 246,011 bytes for 819,418 original bytes.

The proposed page sketch does not yet include the four-byte total decoded size from this measured layout.
That field permits exact allocation for full-page decode without individual decoded lengths.
Its location in the Loki page format remains open.
The optimized kernel does not change the compressed payload or symbol table.

The Zstd blocks contain two-byte string lengths.
Their directory costs eight bytes per block, plus a final four-byte offset.
Zstd frame checksums remain enabled.
The FSST benchmark adds no checksum.
These differences prevent direct comparison with the final Loki serialized format.

The throughput timer excludes table training and construction.
Fresh training for each Loki output page remains outside these measurements.
Loki ingestion and selective-query performance remain unmeasured.
Only ARM64 throughput is measured for the optimized kernel.

The FSST raw samples reside in `results/kernel-upstream.json`, `results/kernel.json`, and `results/kernel-repeat.json`.
The access samples reside in `results/kernel-access.log` and `results/kernel-upstream-access.log`.
The earlier Zstd samples remain in `results/tuned.json` and `results/repeat.json`.
The original HTML report records the earlier experiment and excludes the optimized fork.

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
- Where the page stores its total decoded size for exact page allocation.

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
