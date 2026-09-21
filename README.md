# glean

A daily curated list of soon expiring domain names.

Joins a wordlist with a live signal and compiles the interesting part: names
whose `.com`, `.net`, `.org` or `.xyz` registration is actually on its way to
being deleted.

A domain does not become available on its expiry date -- it walks the Registry
Grace Period pipeline of RFC 3915, and a list built on "expires next Tuesday"
is close to useless. What this reports on is the RGP status codes, which say
where in that pipeline a name really is. Hence `glean`: gleaners gather what is
left in the field after the harvest, and this gathers the names the registries
are about to let go.

```
$ glean list
DOMAIN        STAGE             DROPS        EXPIRY      REGISTRAR
lummox.net    available         -            -           -
quiddity.com  pendingDelete     2026-09-11   2026-06-27  GoDaddy.com, LLC
fustian.org   redemptionPeriod  2026-10-06   2026-07-01  NameCheap, Inc.
```

## Installation

```
$ go install github.com/miku/glean@latest
```

## Usage

```
$ glean sources        # the word lists, and what each one will cost
$ glean words          # the candidate labels those lists produce
$ glean scan           # look up everything due a check
$ glean list           # what is dropping, soonest first
$ glean stats          # summarise the store
$ glean prune          # drop records no source covers any more
```

`glean help <command>` has the flags and the long form, and
`glean completion bash|zsh|fish` prints a completion script.

The first scan is the expensive one. After that, `scan` only looks up what the
schedule says is due, which settles at a few thousand lookups a day -- see
[Why the second scan is cheap](#why-the-second-scan-is-cheap).

```
$ glean scan
2 sources, 92523 labels, 3 req/s per endpoint, store ~/.local/state/glean/domains.jsonl.zst
370092 domains to check, 0 up to date, 0 in store
1761/370092  8.9/s  taken=1363 avail=334 unknown=0 fail=0 throttle=0  eta=11h29m
```

It is resumable. The store is written every minute and on exit, so `^C` costs
at most a minute of lookups and the next run picks up where this one stopped.

## What "expiring soon" actually means

A domain does not become available on its expiry date. It walks a fixed path,
and only the last two steps of it are worth anything:

```
expiry date
  -> autoRenewPeriod   0-45 days   registrar may still reverse, usually does not
  -> redemptionPeriod  30 days     only the registrant can restore, for a fee
  -> pendingDelete      5 days     nothing can save it
  -> drops
```

So a list built on "registry expiry date is next Tuesday" is close to useless:
the great majority of those registrations renew, and the ones that do not are
unbuyable for another ten weeks. What this tool watches instead is the EPP
status codes, which say where in that pipeline a name actually is. `list`
reports:

| stage              | meaning                                                         |
| ------------------ | --------------------------------------------------------------- |
| `available`        | unregistered right now                                            |
| `pendingDelete`    | drops within five days, and the date is predictable               |
| `redemptionPeriod` | drops in about 35 days unless the registrant pays to restore it   |
| `lapsed`           | term ended, registry publishes no RGP status; somewhere in the pipeline |
| `autoRenewPeriod`  | just renewed; the registrar has 45 days to hand it back (`--all`)  |

`autoRenewPeriod` is the one to be careful with, and the trap that RDAP sets
for you. It does not mean "expired". The registry has already renewed the name
and pushed its expiry date a year into the future; the status only records that
the registrar can still return the registration for a refund. Most do not. It
is a lead rather than a listing, so it is behind `--all`, and its drop date is
projected from the renewal event -- never from the expiry date, which is now a
year out.

Dates printed with a `~` are projected across a stage whose length the
registrar chooses, and are soft by weeks. Dates without one are anchored to an
observed status transition and are good to a day or two.

## Why the second scan is cheap

The expiry date is not the signal, but it is what makes the tool affordable to
run. Every lookup answers not just "is this dropping" but "when could it
possibly start dropping", and a name whose term runs to 2034 needs no attention
until 2034.

So `scan` skips whatever is not due, and what is due depends on where the name
sits:

| state                        | rechecked      |
| ---------------------------- | -------------- |
| `pendingDelete`              | daily          |
| `redemptionPeriod`           | every 3 days   |
| `autoRenewPeriod` / `lapsed` | every 5 days   |
| available                    | weekly         |
| registered, term ending soon | every 3 days   |
| registered, long term        | a week before the term ends, capped at a year |
| repeated lookup failures     | 1h, doubling to a week |

A 300k-domain store therefore costs one long scan and then a few thousand
lookups a day, which is minutes rather than hours.

## Rate limits

The scan is paced per registry *host*, not per TLD -- `.com` and `.net` are one
machine reached through two URL paths, and limits are enforced per client
address. The pacing adapts: a throttling response cuts the rate for that host,
a run of clean ones walks it back toward `--rate`.

Registries signal overload in different ways, and not all of them by the book.
Verisign answers `429`. PIR (`.org`) sheds load with a **`403` from an AWS load
balancer**, HTML body, no `Retry-After`, for a request it served a second
earlier and will serve again a second later. Both are treated as "slow down"
rather than as refusals, because reading PIR's 403 as permanent marks a run of
perfectly good domains as failures and, worse, does not slow down.

What the default set actually tolerates, measured rather than guessed:

| endpoint                                | observed                                                        |
| --------------------------------------- | --------------------------------------------------------------- |
| `rdap.verisign.com` (com/net)           | comfortable at 6/s                                                |
| `rdap.publicinterestregistry.org` (org) | ~400 requests at 2/s, then steady shedding; sustained rate is under 1/s |
| `rdap.centralnic.com` (xyz)             | same shape as PIR                                                 |

The limit is not really a rate. A burst of 400 requests at 6/s passes cleanly
at any concurrency, and it is the *sustained* run that degrades -- a token
bucket with a generous burst and a slow refill. Neither concurrency
(`--per-host`) nor HTTP/2 was found to matter.

This is why `--min-rate` defaults as low as it does. PIR's sustained allowance
sits below one request per second; a higher floor leaves the limiter pinned at
the bottom *and still throttled*, burning retries and marking perfectly good
domains as failures. The limiter needs room to find the real number.

The practical consequence: `.com` and `.net` scan fast, `.org` and `.xyz` are
slow no matter how you ask. Bound a run with `scan --limit 20000` and let a first
scan take a few days; the schedule and the resumable store are built for
exactly that.

## Data storage

One zstd-compressed JSON Lines file, sorted by domain, rewritten in full and renamed
into place on every checkpoint. It lives in the XDG *state* directory rather
than the cache directory -- a wiped cache is an inconvenience, a wiped scan is
five hours.

```
$ zstdcat ~/.local/state/glean/domains.jsonl.zst | head -1
{"domain":"aalii.com","status":"taken","epp":["clientTransferProhibited"],
 "expiry":"2027-06-23","changed":"2026-05-27",
 "registrar":"TurnCommerce, Inc. DBA NameBright.com","checked":"2026-09-07T12:23:14Z"}
```

At a few hundred thousand records this is tens of megabytes and a full rewrite
costs well under a second, which buys atomicity and a file that diffs cleanly
between runs. A database can wait until the data says it is needed.

zstd rather than gzip because nearly every command begins by loading the whole
store, and zstd decompresses it about five times faster (~20ms vs ~100ms for
300k records) at a slightly smaller size. `--store` also takes a `.gz` or an
uncompressed path; reading goes by the file's magic bytes, writing by its
suffix. A store from before the switch, `domains.jsonl.gz`, is still read when
no `.zst` exists, and the next scan writes it out as `domains.jsonl.zst` (the
old file is left alone).

The word lists are configuration, not data, and live in the XDG *config*
directory instead -- `~/.config/glean/sources.d/`. They are inputs you
write; the store is what the tool accumulates.

## How it works

Everything is RDAP (RFC 7480/9082/9083), and only RDAP. Every gTLD worth
scanning publishes an RDAP service, and RDAP answers the two questions this
tool asks -- is it registered, and where is it in its lifecycle -- as typed JSON
fields:

* **Registered or not** is the HTTP status. `404` means the name is free; there
  is nothing to pattern-match.
* **Lifecycle** is the `status` array, normalised from RFC 9083's lowercase
  spelling (`"pending delete"`) to the EPP one everybody writes
  (`pendingDelete`). Registries emit both; the store keeps one.
* **Dates** come from the `events` array, trimmed to `YYYY-MM-DD`. Registries
  publish them at wildly different precisions and none of it means anything for
  a lifecycle measured in days.
* **Endpoints** come from IANA's bootstrap registry (RFC 9224), cached beside
  the store for a week, with a built-in fallback for the four default TLDs so a
  scan can start without reaching IANA.

The sibling tool [tldhunter](https://github.com/miku/tldhunter) does the other
half of this problem -- one keyword against 1438 TLDs, with whois fallback and
the pile of per-registry regexes that whois requires. It is the interactive
tool; this is the batch one, and the narrow TLD set is what lets it skip whois
entirely.

Standard library only, no dependencies.

## Word lists

Out of the box the candidate list is web2 -- Webster's Second International,
whose 1934 copyright has elapsed -- filtered to entries that are already
lowercase ASCII of the right length. That drops proper nouns, which web2 is
full of and which make poor generic domains, along with the accented and
hyphenated entries, which are not registrable as written.

web2 is compiled into the binary rather than read from
`/usr/share/dict/words`, because that path is not the same file everywhere: it
is web2 on the BSDs and macOS, but usually the much smaller `american-english`
on Debian. The same command would otherwise produce 74,947 candidates on one
machine and 34,912 on another, and a store built on the first would look
three-quarters orphaned to the second. Pass `--dict /usr/share/dict/words`, or
write `include:` instead of `builtin:`, if you would rather have this host's.

```
$ glean words --count           # 74947
$ glean words --min 4 --max 6   # 27939 shorter, better ones
```

That is a fine default and a poor ceiling. Three- and four-letter names are the
interesting ones and the dictionary has almost none of them, so the list is
extensible through a directory of small files:

```
~/.config/glean/sources.d/
  10-web2.txt              web2, compiled in
  20-letters3.txt          every three-letter string
  30-alnum3.txt            three characters, letters and digits
  40-pronounceable5.txt    five letters, consonant-vowel alternating
  50-letters4.txt          every four-letter string
  60-compound.txt          two common words, "word" + "cloud"
```

`glean sources --init` writes that directory, with everything past the
three-letter list switched off and the arithmetic for why in each file. The
lists have different lifecycles -- web2 has not changed since 1934, a surname
list is regenerated from a census dump now and then, an enumeration of every
four-letter string is not a file at all but a loop -- and a directory of small
files lets each be added, refreshed or disabled without touching the others.

### The file format

A source file is a word list whose *leading* comment block may carry
directives. Leading only: a list downloaded from elsewhere may have `#` comments
scattered through it, and none of them should be able to change how the file is
read.

```
# 30-airport-codes.txt -- IATA, three letters, high recall
# tlds: com
# priority: 30
ord
lhr
nrt
```

A file may instead name a generator, and carry no words at all:

```
# 20-letters3.txt
# generate: letters 3
# tlds: com,net,org,xyz
```

`generate:` takes `letters N`, `alnum N`, or `pattern` over the classes `C`
(consonant), `V` (vowel), `L` (letter), `D` (digit) and `N` (alphanumeric) --
so `pattern CVCVC` is the pronounceable five-letter names.

`compound` pairs words rather than characters, for the two-word names:
`wordcloud`, `linktree`.

```
# 60-compound.txt
# generate: compound common
# max: 12
# tlds: com
```

`common` is a list compiled into the binary: about 1250 common English words
in rough order of frequency, with the function words taken out. Every ordered
pair of those is 1.5 million labels at up to twelve letters, which is six days
against `.com` -- a lot, and the reason the pairs do not come out in
alphabetical order. They come out by the sum of the two words' ranks, so all
the pairs of the first ten words come before any pair that uses the
thousandth. A nightly `scan --limit` that only gets partway through has then spent
its budget on the best pairs, not on everything starting with "ace".

`compound left.txt right.txt` pairs two lists of your own instead, one for each
side, which is how you get `my` + anything or anything + `hub` without also
getting `hubmy`. Each file is read in order, most important word first, and
`min:` and `max:` bound the length of the whole label. Keep those lists in a
subdirectory, `sources.d/lists/` say: a word list directly in `sources.d` is
read as a source of its own and scanned as single words.

Or it may point at a list that lives somewhere else and is maintained by
something else, which is how a list that updates on its own schedule stays that
way. The stub in `sources.d` carries the policy; the target is somebody else's
business:

```
# 40-surnames.txt -- refreshed nightly by cron, do not edit the target
# include: /var/lib/wordlists/census-surnames.txt
# fold: false
# min: 4
# tlds: com,net
```

| directive   | meaning                                                          |
| ----------- | ---------------------------------------------------------------- |
| `tlds`      | TLDs to pair this list with; defaults to `scan --tlds`             |
| `priority`  | lower is scanned first; defaults to the `NN-` filename prefix      |
| `generate`  | enumerate rather than read a file                                  |
| `builtin`   | read a list compiled into the binary (`web2`, `common`)            |
| `include`   | read labels from a file elsewhere                                  |
| `min`, `max`| label length bounds                                                |
| `fold`      | lowercase entries and keep them (default), or drop capitalised ones |
| `enabled`   | `false` leaves the file in place but out of the scan               |

### What it costs

The one question worth answering before enabling a list is what the first pass
will cost, which is what `sources` is for:

```
$ glean sources
SOURCE                PRI  SPEC                   LABELS  TLDS             DOMAINS  NEW      DUE   FIRST PASS
web2                  10   builtin:web2           74947   com,net,org,xyz  299788   0        1674  -
letters3              20   letters 3              17576   com,net,org,xyz  70304    70304    0     3.3h
alnum3 (off)          30   alnum 3                46656   com,net,org,xyz  116320   116320   0     5.4h
pronounceable5 (off)  40   pattern CVCVC          231525  com              229618   229618   0     21.3h
letters4 (off)        50   letters 4              456976  com              452616   452616   0     41.9h
enabled                                                          370092   70304    1674  3.3h
```

Every file in the directory is listed, the switched-off ones marked `(off)`,
because the question the table answers is what turning one on would cost. Pass
`--on` for the enabled ones alone.

`NEW` is the column to read, and it is marginal: labels this source contributes
that are not already in the store and were not already contributed by an
earlier *enabled* source. A disabled row is priced independently against the
enabled set rather than against the other disabled ones, since you would turn
them on one at a time -- so those rows do not sum, and the `enabled` total
counts only what a scan would actually do.

The marginal part matters: `alnum3` yields 46,656 labels but only 116,320 new
domains rather than 186,624, because `letters3` already covered 17,576 of them.
`FIRST PASS` is that divided by the rate the registry will tolerate, counting
`.com` and `.net` against one budget because they are one machine.

The recurring cost is unrelated and much smaller. A registered name with a
distant expiry date is not looked at again until shortly before that date, so
the steady state is a few thousand lookups a day almost regardless of how long
the list is -- see [Why the second scan is cheap](#why-the-second-scan-is-cheap).
What a big list costs is the one-off.

### Spending a short budget

`scan --limit` spends its budget in source priority order rather than spreading it
evenly. This matters as soon as there is more than one list: an even split
gives the largest share to the largest source, which is the four-letter
enumeration, and a nightly run that grinds through that while the curated
lists go unchecked has it backwards. A level is taken whole, and only the level
the budget runs out on is split across the endpoints -- so a short run still
touches every registry rather than finishing one TLD and never reaching the
rest.

```
$ glean scan --limit 50000   # a night's worth, best lists first
```

### Provenance

`list --source letters3` filters by which list a name came from. That is
answered by asking the source whether it contains the label, not by a tag in
the store: the store stays a record of what the registries said, and editing a
word list never leaves stale provenance behind in 300k records.

Deleting or narrowing a list leaves records nothing covers any more. They cost
nothing -- `scan` stops scheduling them, so they are never looked up again --
but `stats` counts them and `prune` removes them:

```
$ glean prune          # dry run, reports what it would delete
$ glean prune --force  # actually delete
```

`scan --wordlist list.txt` still takes a single curated list and bypasses `sources.d`
entirely, and with no `sources.d` at all the tool behaves exactly as it did
before the directory existed: web2, four to eight letters, four TLDs.

## License

MIT
