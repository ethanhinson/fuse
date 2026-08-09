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
//go:generate echo "reputation snapshots are hand-curated; refresh manually from the pinned sources listed above (no network fetch in build/test)"
