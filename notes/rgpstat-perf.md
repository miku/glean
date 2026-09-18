# perf: storage and query

Notes from 2026-09-18, after the store grew past a million records.

## gzip -> zstd

Nearly every command starts by loading the whole store, so decompression is
paid on every run. Measured on a synthetic 300k-record store shaped like the
real one (54MB raw):

| format                         | size    | decompress | compress |
|--------------------------------|---------|------------|----------|
| gzip (default level)           | 9.2 MB  | ~100 ms    |          |
| zstd SpeedFastest              | 9.5 MB  | ~25 ms     | ~60 ms   |
| zstd SpeedDefault              | 9.1 MB  | ~20 ms     | ~75 ms   |
| zstd SpeedBetterCompression    | 8.3 MB  | ~20 ms     | ~140 ms  |
| zstd SpeedBestCompression      | 8.2 MB  | ~20 ms     | ~620 ms  |

zstd decoding speed barely depends on the level, so the store is written at
SpeedBetterCompression: small file, fast read, and the extra encoding time is
paid once per checkpoint instead of on every load. (Synthetic data without
entropy -- sequential names, fixed dates -- compresses 40x and makes every
codec look the same; the benchmark needs random labels and timestamps to be
meaningful.)

The default store is now `domains.jsonl.zst`. Reading detects the format from
the file's magic bytes and writing picks it from the suffix. A missing `.zst`
falls back to the `.gz` next to it and is migrated on the next flush.

## where the time goes now

`rgpstat stats` on mira, 1,260,410 records, 25.4MB zstd:

    real 3.3s   user 5.3s   sys 0.1s

On a synthetic store of the same size (on an M-series Mac, so faster in
absolute terms), `stats` breaks down as:

| phase                                          | time    |
|------------------------------------------------|---------|
| openStore (zstd + JSON + map insert)           | ~1.2 s  |
| st.All(): copy every record and sort by domain | ~0.45 s |
| stage/due loop (reparses date strings)         | ~0.25 s |
| countOrphans                                   | ~0.15 s |

The CPU profile is mostly Go runtime work: madvise, page faults and GC span
scanning. zstd and JSON decoding barely show up, and user > real means the
garbage collector is working in parallel. Each `Record` is about 150 bytes with
5-6 separate string allocations plus an EPP slice. `stats` holds 1.26M of them
in the map *and* a full sorted copy from `All()`, which comes to several
hundred MB of pointer-heavy heap. Setting GOGC=400 did not help, so the cost is
mostly allocating and touching memory, not collection frequency.

Decompression is no longer the bottleneck. The in-memory representation is.

## options

### tier 1: no format change (do this first)

1. **Don't sort when order doesn't matter.** `stats`, `countOrphans` and scan
   planning can loop over the map directly. This saves ~0.45s and half the
   peak memory.
2. **Store records compactly in memory.** Status and stage as `uint8`, EPP as a
   bitmask, expiry/changed/checked as day numbers or unix times, and registrar
   as an index into a table of names (only a few thousand distinct). Domains
   can go into one shared byte buffer with offsets. That is about 32 bytes per
   record with almost no pointers, so the GC has little to scan. It also takes
   the date parsing out of `stage()` and `due()`. Convert to `Record` only at
   the edges (JSON output, `list`).
3. **Parse in parallel.** The file is sorted JSON lines: cut the decompressed
   stream into chunks on newline boundaries and decode them on N goroutines.
4. **Keep a slice instead of a map.** The file is already sorted, so it can
   load straight into a sorted slice. Lookups use binary search or a
   `map[string]int32` index.

Rough expectation: `stats` at 0.5-0.8s on mira, and the store stays a
greppable `.jsonl.zst`. Tier 1 is also the prerequisite for tier 2.

### tier 2: a derived file next to the JSONL

5. **Binary snapshot.** JSONL stays the source of truth. At every `Flush`, also
   write the compact tier-1 layout (a header plus a few large arrays). Loading
   is then little more than reading it into memory, perhaps ~100ms. The cost is
   keeping two files consistent: validate the snapshot against the JSONL's
   mtime or a hash on open, and rebuild it when stale.
6. **Summary sidecar.** At flush, write counts by TLD, by stage, and a histogram
   of due dates. "Due now" is then the sum of the histogram up to today, and
   `stats` becomes near-instant. Stages that depend on time (lapsed) need their
   own date histogram. This only helps `stats`; `list`, `scan` and `prune`
   still load everything.

### tier 3: a real database

7. **SQLite**, via `modernc.org/sqlite` (pure Go, ~2x slower) or
   `mattn/go-sqlite3` (cgo).
   - Checkpoints write only the changed rows. Today every checkpoint rewrites
     all 1.26M records, a few seconds of CPU per minute during a scan, and
     that grows linearly with the store.
   - `list` filters become indexed queries, and `stats` a GROUP BY of a few
     hundred ms.
   - You lose the diffable `zstdcat | grep` file (a `dump` command could bring
     it back), and you take on a large dependency.
8. **DuckDB or Parquet.** Aggregations would be very fast, but the workload is
   a scan updating single rows continuously, which these handle badly, and
   DuckDB needs cgo. Parquet is at most worth having as an *export* for
   offline analysis.

## tier 1, as done

For scale: `zstdcat -T0` reads the whole mira store in 0.14s, so everything
above that is ours.

- The store is a `[]Record` in domain order plus a `map[string]int32` index.
  The file is sorted, so loading is appending. Filling the index costs ~60ms,
  against ~200ms for the old `map[string]Record`: Go maps store values over
  128 bytes out of line, which is one more allocation per record.
- Put appends new domains unsorted. `sort()` sorts that tail and merges it
  in: linear, ~0.2s at 1.26M, run by Flush and by All. A file that is not
  strictly sorted (hand-edited, duplicates) is loaded through Put instead, so
  the later line wins.
- `All()` is an `iter.Seq[Record]` over the live slice under the lock, not a
  sorted copy. Flush writes from the live slice too: the scan loop is both
  the only writer and the caller of Flush, so holding the lock blocks nothing.
- `readRecords` cuts the decompressed stream into 1MB blocks at newlines and
  decodes them on GOMAXPROCS goroutines. Line numbers in errors survive.
- `parseDate` is written out by hand. `stage` and `due` call it about twice
  per record, and `time.Parse` was most of their cost.

Measured on a synthetic 1.26M-record store, on the Mac:

| | before | after |
|---|---|---|
| openStore | ~1.2 s | ~0.5 s |
| All() | ~0.45 s (copy + sort) | 0 (iterates in place) |
| stage/due loop | ~0.25 s | ~0.15 s |
| `rgpstat stats`, end to end | ~2.2 s | ~1.0 s |

Not done: compact records (item 2). Load is now mostly allocation and page
faults for 152-byte records with 5-6 strings each, plus one copy when the
decoded blocks are concatenated. Shrinking `Record` is the next lever, but it
touches every file that uses one, so it waits until it is needed.

## recommendation

Tier 1 first. It attacks the real cost (allocation and GC) and changes nothing
on disk. If checkpoint cost during long scans or ad-hoc queries in `list`
become the pain point after that, SQLite is the principled next step. The
binary snapshot is only worth it if sub-100ms loads matter while the JSONL
stays the source of truth.
