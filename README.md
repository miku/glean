# expiringsoon

A daily curated list of soon expiring domain names.

Joins a static wordlist with a live signal and compiles the interesting part:
dictionary words whose `.com`, `.net`, `.org` or `.xyz` registration is on its
way to being deleted.

```
$ expiringsoon list
DOMAIN        STAGE             DROPS        EXPIRY      REGISTRAR
lummox.net    available         -            -           -
quiddity.com  pendingDelete     2026-09-11   2026-06-27  GoDaddy.com, LLC
fustian.org   redemptionPeriod  2026-10-06   2026-07-01  NameCheap, Inc.
```

## Installation

```
$ go install github.com/miku/expiringsoon@latest
```

## Usage

```
$ expiringsoon words          # candidate wordlist from /usr/share/dict/words
$ expiringsoon scan           # look up everything due a check
$ expiringsoon list           # what is dropping, soonest first
$ expiringsoon stats          # summarise the store
```

The first scan is the expensive one. After that, `scan` only looks up what the
schedule says is due, which settles at a few thousand lookups a day -- see
[Why the second scan is cheap](#why-the-second-scan-is-cheap).

```
$ expiringsoon scan
74947 words x 4 TLDs, 3 req/s per endpoint, store ~/.local/state/expiringsoon/domains.jsonl.gz
299788 domains to check, 0 up to date, 0 in store
1761/299788  8.9/s  taken=1363 avail=334 unknown=0 fail=0 throttle=0  eta=9h18m
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
| `autoRenewPeriod`  | just renewed; the registrar has 45 days to hand it back (`-all`)  |

`autoRenewPeriod` is the one to be careful with, and the trap that RDAP sets
for you. It does not mean "expired". The registry has already renewed the name
and pushed its expiry date a year into the future; the status only records that
the registrar can still return the registration for a refund. Most do not. It
is a lead rather than a listing, so it is behind `-all`, and its drop date is
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
a run of clean ones walks it back toward `-rate`.

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
(`-perhost`) nor HTTP/2 was found to matter.

This is why `-minrate` defaults as low as it does. PIR's sustained allowance
sits below one request per second; a higher floor leaves the limiter pinned at
the bottom *and still throttled*, burning retries and marking perfectly good
domains as failures. The limiter needs room to find the real number.

The practical consequence: `.com` and `.net` scan fast, `.org` and `.xyz` are
slow no matter how you ask. Bound a run with `scan -n 20000` and let a first
scan take a few days; the schedule and the resumable store are built for
exactly that.

## Data storage

One gzipped JSON Lines file, sorted by domain, rewritten in full and renamed
into place on every checkpoint. It lives in the XDG *state* directory rather
than the cache directory -- a wiped cache is an inconvenience, a wiped scan is
five hours.

```
$ zcat ~/.local/state/expiringsoon/domains.jsonl.gz | head -1
{"domain":"aalii.com","status":"taken","epp":["clientTransferProhibited"],
 "expiry":"2027-06-23","changed":"2026-05-27",
 "registrar":"TurnCommerce, Inc. DBA NameBright.com","checked":"2026-09-07T12:23:14Z"}
```

At a few hundred thousand records this is tens of megabytes and a full rewrite
costs well under a second, which buys atomicity and a file that diffs cleanly
between runs. A database can wait until the data says it is needed.

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

## Wordlist

The default candidate list is `/usr/share/dict/words`, filtered to entries that
are already lowercase ASCII of the right length. That drops proper nouns, which
web2 is full of and which make poor generic domains, along with the accented
and hyphenated entries, which are not registrable as written.

```
$ expiringsoon words -c              # 74947
$ expiringsoon words -min 4 -max 6   # 27939 shorter, better ones
```

`scan -w list.txt` takes a curated list instead, one word per line, `#` for
comments -- for compound and coined words, which the dictionary will never have.

## License

MIT
