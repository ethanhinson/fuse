package reputation

// Pinned upstream data sources for the embedded reputation snapshots.
//
// Pin date: 2026-08-09
//
// Licensing decision (MIT-only for code; permissively/attribution-licensed
// data only — NO GPL data is shipped):
//
//   - Blocklist (hosts format, `0.0.0.0 domain`):
//       StevenBlack/hosts — MIT
//       https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
//
//   - Blocklist (one-domain-per-line):
//       Peter Lowe's Ad and tracking server list
//       https://pgl.yoyo.org/as/serverlist.php?hostformat=hosts;showintro=0
//
//   - Popularity allowlist (Majestic-Million-style CSV):
//       Majestic Million — CC BY 3.0
//       https://downloads.majestic.com/majestic_million.csv
//
// DROPPED: hagezi TIF (GPL-3.0) — excluded to keep the bundle GPL-free.
//
// The files under data/ are small, hand-authored, committed representative
// subsets in the upstream formats. They are refreshed manually; nothing here
// fetches over the network at build or test time. When refreshing, download
// the sources above, extract a bounded curated subset in the matching format,
// and keep the accompanying LICENSE-*.txt attribution files in sync.
//
// ── data/popularity.csv IS AN AUTHORIZATION SOURCE ──────────────────────────
//
// Read this before refreshing it. KnownGood is no longer only a classifier
// hint: since change 0069 the permissions web_fetch floor promotes a KnownGood
// host to a real AUTO-APPROVE — VerdictAllow with no classifier call and no
// human prompt, for any path and query string under that host. Adding a row to
// this CSV therefore GRANTS AUTHORIZATION. It is a security change wearing the
// costume of a data refresh.
//
// Two things constrain that, and a refresher must engage with both:
//
//  1. exfilShapeDenylist (internal/permissions/fetchhost.go) subtracts exfil
//     shapes — paste and upload services, link shorteners, webhook and
//     request-capture endpoints — from the promotion, so those cannot be
//     promoted even if a refresh imports them. It is a FLOOR, NOT EXHAUSTIVE.
//     A newly imported host of that shape which is not yet listed there WILL
//     be promoted. When you spot one, add it to that denylist.
//
//  2. TestKnownGoodPromotion_PinnedSet (internal/permissions,
//     fetchhost_promotion_test.go) pins the exact set of hosts this CSV
//     currently promotes. Any refresh that widens authorization turns the
//     suite red on purpose. DO NOT update the pin to make the build pass.
//     Review each newly authorized host as a grant first — ask whether an
//     attacker-chosen URL under it could exfiltrate data or serve
//     attacker-controlled content — then update the pin deliberately, and say
//     in the commit message which hosts you granted and why.
//
// Keep the subset genuinely bounded and curated: prefer a small number of
// operator-run sites whose whole namespace is safe to fetch unreviewed over a
// mechanical top-N slice of the upstream file. bit.ly and pastebin.com are
// present in the current snapshot deliberately — they are real top sites and
// real exfil shapes, and they exist here so the denylist's subtraction stays
// under test.
//
// ads-and-popular.example is likewise deliberate, and is the one SYNTHETIC row
// in popularity.csv: it also appears in data/blocklist-domains.txt, so it is a
// host that is simultaneously "popular" and blocked. Real refreshed data
// overlaps that way routinely (doubleclick.net is a Majestic top-100 domain and
// sits in essentially every blocklist), and that overlap is what pins
// "blocklist beats the known-good promotion" in classifyFetchHost. Do not drop
// either row on a refresh — TestBlocklistBeatsKnownGoodPromotion fails loudly if
// you do.
//
// The blocklist files carry no such hazard: they only ever DENY, so a wider
// snapshot narrows what is reachable rather than widening it.
//
//go:generate echo "reputation snapshots are hand-curated; refresh manually from the pinned sources listed above (no network fetch in build/test)"
