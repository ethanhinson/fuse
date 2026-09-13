---
name: interpolated-filename-into-regex
slug: interpolated-filename-into-regex
title: A filename interpolated into a regex silently matches too much — compare as a literal field
hook: "Interpolating a filename into grep/awk/sed regex makes every '.' a wildcard — match the field literally (awk $2 == name), never as an ERE"
promotion_state: candidate
changes: [82]
created: 2026-09-13
updated: 2026-09-13
topics: [shell, posix, regex, security, checksums]
---

Shell code that looks a filename up inside a manifest — a checksums file, a lockfile, an index —
routinely interpolates that name straight into a regex: `grep -E "$name"`, `awk "/$name/"`. In a
dot-dense release artifact name (`fuse_0.1.0_darwin_arm64.tar.gz`) every `.` becomes "any
character", so the pattern matches a whole family of unrelated lines. The failure is silent and
*over-permissive*: it does not error, it matches more.

The concrete instance (change 0082, installer checksum verification): the matcher existed to
**refuse duplicate entries** for the requested archive. With the name interpolated unescaped, a
checksums file naming `fuse_0P0P0-SNAPSHOT-…_darwin_arm64PtarPgz` matched the pattern for the real
`.tar.gz` name, the duplicate-entry refusal never fired, and the installer printed "checksum
verified" while installing unrelated binaries. The exact guard the function existed to provide was
defeated by its own lookup.

**Rule:** never interpolate an externally-supplied name into a regex for an equality test. Compare
the field literally:

```sh
# wrong — '.' is a wildcard, and the duplicate guard is defeated
grep -E "$file" checksums.txt

# right — exact literal field comparison, no regex evaluation of $file
awk -v f="$file" '$2 == f { print $1; n++ } END { exit(n == 1 ? 0 : 1) }' checksums.txt
```

**Testing rule:** a security guard of this shape must be proven by *reproducing the bypass first*.
A test that only feeds the guard a well-formed name passes identically against the broken and the
fixed code. Construct the adversarial name (substitute the dots), watch the old code accept it,
then fix.
