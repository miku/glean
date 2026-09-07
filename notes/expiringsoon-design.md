# design: expiring soon

Join a list of words (static) with a live signal (expensive) and compile
(filter) a list of useful data for people search for interesting domains, that
are soon expiring.

There is a wordlist in @/usr/share/dict/words on many unix systems (including this one).

> https://unix.stackexchange.com/questions/286787/who-or-what-compiled-usr-share-dict-words

 File:  [cvs.NetBSD.org] / src / share / dict / README
Revision 1.1: download - view: text, annotated - select for diffs
Sun Mar 21 09:45:37 1993 UTC (33 years, 5 months ago) by cgd
Branches: MAIN
CVS tags: HEAD

Initial revision

#   @(#)README  5.1 (Berkeley) 5/7/91

WEB ---- (introduction provided by jaw@riacs) -------------------------

Welcome to web2 (Webster's Second International) all 234,936 words worth.
The 1934 copyright has elapsed, according to the supplier.  The
supplemental 'web2a' list contains hyphenated terms as well as assorted
noun and adverbial phrases.  The wordlist makes a dandy 'grep' victim.

     -- James A. Woods    {ihnp4,hplabs}!ames!jaw    (or jaw@riacs)


## tool: tldhunter

I am the author and maintainer of a tool for looking up domain names, called tldhunter, full source code can be found here: @~/code/miku/tldhunter/ and usage is like this:

```
$ tldhunter -h
Usage: tldhunter -k <keyword|domain> [-e <tld> | -E <tld-file>] [-x] [--update-tld]
Without -e or -E, the built-in TLD list (1438 entries) is used,
unless the keyword already ends in a known TLD, which checks just that domain.
Results are cached in /Users/tir/.cache/tldhunter for 24h0m0s (1h0m0s if available; -ttl 0 to disable).
Example: tldhunter -k linuxsec
       : tldhunter -k delta.sh
       : tldhunter -k linuxsec -E tlds.txt
       : tldhunter --update-tld
       : tldhunter --clear-cache
```

## data storage

We can start with a single compressed JSON lines file, that is updated
atomically. Note the name, domain name, expiration data and the last checked
date. We need to only to this once for a large list of words and then we can
already print out the soon expiring list. Once we have an initial scan of say
something between 10K and 100K domains, we can limit additional checks to
domains that are actually soon expiring.

## algorithm proposal

Limit to 3-5 high value TLDs first, e.g. .com, .net, .org, .xyz. Compile a
wordlist, e.g. top N words and some cool sounding compound words (compile this
list first); iterate over wordlist and create or update the expiringsoon.json
file. Batch updates and the update the file atomically. We can hold off to
using a database until we have seen more data.

## tool

The CLI to gather the data and store the results should be written in Go. The
data should be kept in a XDG state directory/file (cache would get wiped after
restart). Can reuse `tldhunter` or start with using it as a command line tool,
or start making changes to the tool to fit our purpose better.


