# FSST page prototype

This prototype tests the page format from the draft LID, “FSST encoding for log message pages.”
Message columns use FSST with no outer compression.
Other binary columns retain their current encoding.

Each page contains its existing presence header and bitmap, followed by:

- A serialized FSST table.
- Little-endian symbol offsets, with one extra terminal offset.
- Little-endian decoded lengths.
- The encoded symbol sequences.

NULL rows have no directory entries.
Empty strings have equal adjacent offsets and a zero decoded length.
The decoder obtains the non-NULL count from the page descriptor.
The prototype uses a type assertion in the page reader to avoid changes across all decoder constructors.

The encoder owns copies of input bytes until flush.
Flush trains one table for the page and encodes all non-NULL values.
Page estimates include buffered input bytes and directory overhead.
Uncompressed statistics count decoded string bytes plus the presence envelope.
Stored sizes and CRC32C cover the complete encoded page.

The decoder retains the encoded page and allocates output from the stored decoded lengths.
It calls DecodeBatch for each requested batch and returns allocator-owned columnar arrays.
The offsets permit independent string access.
The page reader still decodes and discards skipped rows.

The dependency uses the FSST fork at ee30e95db62e, which matches the draft benchmarks.
The vendor directory contains the dependency for normal repository builds.

## Scope limits

This branch demonstrates format feasibility.
It does not establish an end-to-end performance benefit.
It omits format rollout controls, compatibility negotiation, and production hardening.
Older readers cannot read the new encoding.
Small pages can grow because the table and two integer arrays add overhead.
The prototype has no fallback to Zstd for those pages.
Training cost, memory reuse, and realistic query benchmarks require separate evaluation.
