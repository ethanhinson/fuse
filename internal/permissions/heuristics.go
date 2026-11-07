package permissions

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// classifyHeuristic is the heuristic permission layer: it classifies a parsed
// command into a Verdict the gate (Task 7) can consume, composing three
// concerns in strict order.
//
//  1. Network egress is a hard risk boundary. Any egress segment (curl, wget,
//     nc, ssh, scp, or a host-qualified git remote) ⇒ VerdictAsk. It is checked
//     first so an egress command can never fall through to a silent allow.
//  2. A compound whose later segments' relative paths were laundered past
//     scoping by an unprovable cd ⇒ VerdictAsk. Path args are scoped against
//     the ORIGINAL root, but the shell resolves them against wherever a
//     preceding cd landed; once any cd in the compound has a target that is
//     not provably the current directory (`$(pwd)`/"$(pwd)"/$PWD/"$PWD") and
//     not provably a literal in-root path, EVERY later relative path is
//     unproven and nothing after it may auto-approve (Defect 1).
//  3. Wholly read-only commands ⇒ VerdictAllow. A read touches nothing outside
//     the process, so no path scoping is required — the existing safe-list
//     classifier (allSegmentsReadOnlySafe) decides.
//  4. Otherwise every segment must prove itself through the SINGLE per-segment
//     decision path (segmentHeuristicVerdict, shared with the audit
//     decomposition so the two can never drift): a redirect write target must
//     stay in-root, a mutating command's path args must resolve in-root, and
//     an unprovable cd (bare, `-`, opaque, env-prefixed) asks when any
//     segment follows it. Anything the heuristic cannot prove in-scope fails
//     toward the human (VerdictAsk), never toward allow.
//
// roots is the allowed mutation-root set: the canonicalized workspace root
// first, then any extra write roots (the per-session scratch dir, config
// write_roots — change 0068). Every root must already be canonicalized
// (filepath.EvalSymlinks) by the caller so containment comparisons are made
// against real paths.
func classifyHeuristic(segments []Segment, roots []string) Verdict {
	if len(segments) == 0 {
		return VerdictAsk
	}

	// 1. Egress boundary wins first — never a silent allow. A curl/wget whose
	// every URL is provably loopback (and whose outputs land in-roots) is the
	// dev-server health-check shape, not egress (change 0068).
	for _, seg := range segments {
		if isEgress(seg) && !isLoopbackFetch(seg, roots) {
			return VerdictAsk
		}
	}

	// 2. Unprovable cd launders every later relative path (Defect 1). This
	//    check must run BEFORE the read-only short-circuit: `cd "$HOME" &&
	//    git status` is all-read-only-safe, but the git status scope is the
	//    real filesystem wherever the cd landed, not the original root. A
	//    proven-current-dir cd (`cd "$(pwd)"`) or a cd whose literal path
	//    arg is in-root changes nothing about the relative paths after it.
	//
	//    The one carve-out is a BARE cd (`cd`) followed only by read-only
	//    segments (`cd && ls -la`, pinned by TestAutoMode_CompoundCdEdgeCases):
	//    reads are location-independent by the read-only fast path, so nothing
	//    launderable follows — the read-only segments are allowed wherever they
	//    land. A bare cd followed by anything that MUTATES or writes, an
	//    opaque-target cd (`cd "$HOME"`), `cd -`, an env-prefixed cd, and a
	//    non-current-dir cd of any other shape followed by ANY segment asks.
	for i, seg := range segments {
		if seg.Name != "cd" || i >= len(segments)-1 {
			continue
		}
		if !cdChangesDirectory(seg, roots) {
			continue
		}
		if len(seg.Args) == 0 && allSegmentsReadOnlySafe(segments[i+1:]) {
			continue // bare cd with only read-only segments after it (justified carve-out).
		}
		return VerdictAsk
	}

	// 3. Wholly read-only ⇒ allow, no scoping needed.
	if allSegmentsReadOnlySafe(segments) {
		return VerdictAllow
	}

	// 4. Mutating, or a cd whose target is not provably the current directory:
	//    every segment must prove itself through the SINGLE per-segment
	//    decision path. segmentVerdicts applies the same per-segment verdicts
	//    (segmentHeuristicVerdict plus the cd-laundering override) that the
	//    audit decomposition and the classifier prompt carry, so the
	//    whole-compound verdict and the per-segment attribution are computed
	//    by one code path and can never drift apart (Defect 3). Any segment
	//    the heuristic cannot prove in-scope ⇒ VerdictAsk.
	for _, sv := range segmentVerdicts(segments, roots) {
		if sv.Verdict != VerdictAllow {
			return VerdictAsk
		}
	}
	return VerdictAllow
}

// cdChangesDirectory reports whether a cd segment has a target that is NOT
// provably null (the shell stays where it is). It is the compound-level
// predicate that decides whether every later relative path is still proven
// against the original roots (Defect 1):
//
//   - The current-directory literals `$(pwd)`, `"$(pwd)"`, `$PWD`, `"$PWD"`
//     change nothing ⇒ false (deterministic allow, sole 0142 widening).
//   - A literal target whose path arg is provably within any root changes
//     nothing that scoping cannot already prove: the historical behaviour
//     (brief: "literal in-root and out-of-root cd keep their behaviour") is
//     preserved. A literal in-root cd may not keep later relative paths
//     in-root, but Defect 1's pins forbid only the UNKNOWN-target laundering
//     — a literal target's mutating segments are evaluated on their own
//     merits exactly as before, and an out-of-root literal asks anyway.
//   - An out-of-root literal expects an ask anyway; classifyHeuristic's
//     segment loop asks it and, because any non-allow sunk the whole compound,
//     the later segments are never reached silently: an out-of-root cd is a
//     non-allow segment, so the compound asks before any later relative path is
//     evaluated. Same terminal outcome as this laundering check; the check is
//     what pins the reason.
//   - EVERY other target — bare `cd` ($HOME), `cd -`, an opaque word
//     (`cd "$HOME"`), an env-prefixed cd (`HOME=/etc cd`), any near-miss of the
//     current-dir forms (`cd "$(pwd)/.."`, `cd "$(cd /; pwd)"`) — is not
//     provably null ⇒ true.
func cdChangesDirectory(seg Segment, roots []string) bool {
	if isCdCurrentDir(seg) {
		return false
	}
	if len(seg.Args) == 0 {
		// Bare cd (goes to $HOME) or cd - (previous dir). The - is a leading
		// '-' word pathArgs drops, so both have no path args and no literal
		// proof: they change the directory.
		return true
	}
	// A literal single-arg cd. The target's scope is evaluated by the segment
	// loop below; a literal in-root target keeps its historical behaviour and
	// a literal out-of-root target asks there. Only the UNKNOWN targets are
	// laundering.
	for _, arg := range pathArgs(seg) {
		if withinAnyRoot(arg, roots) {
			return false
		}
	}
	return true
}

// segmentVerdicts evaluates each parsed segment through the deterministic
// layers (rules, safelist, heuristic) INDEPENDENTLY and returns one
// SegmentVerdict per segment (change 0142). It is the per-segment decomposition
// the gate carries into the classifier's pending-call context and onto the
// Decision event for a compound bash command, so a compound outcome stays
// attributable to the specific part that drove an ask or deny — not just the
// raw string.
//
// The override below mirrors the laundering check in classifyHeuristic
// EXACTLY, so the decomposition can never drift from the whole-compound
// verdict (Defect 1, Defect 3): a cd that changes the directory (bare `cd`,
// `cd -`, an opaque or env-prefixed target — anything cdChangesDirectory says
// is unprovable) and has any non-carved-out segment after it is named as the
// deciding ask, because the compound asks precisely because of it. The
// carve-outs are the matching ones: a current-dir cd changes nothing, and a
// bare cd followed only by read-only segments is the pinned `cd && ls -la`
// allow. A literal in-root cd (`cd x`, `cd .`) never trips cdChangesDirectory
// and so keeps its historical allow with no override.
func segmentVerdicts(segments []Segment, roots []string) []SegmentVerdict {
	out := make([]SegmentVerdict, len(segments))
	for i, seg := range segments {
		v, r := segmentHeuristicVerdict(seg, roots)
		if seg.Name == "cd" && cdChangesDirectory(seg, roots) && i < len(segments)-1 {
			if !(len(seg.Args) == 0 && allSegmentsReadOnlySafe(segments[i+1:])) {
				v = VerdictAsk
				r = LayerHeuristic + ": cd to an unknown target (the paths after it are unproven)"
			}
		}
		out[i] = SegmentVerdict{Name: seg.Name, Verdict: v, Reason: r}
	}
	return out
}

// segmentHeuristicVerdict evaluates one segment through the deterministic
// heuristic layers (egress / read-only / write-target / cd / opaque / kill /
// docker / path scoping, plus the composition gate) and returns its verdict
// and a layer-named reason (change 0142). It is the SINGLE per-segment decision
// path of this file: classifyHeuristic is expressed in terms of it, so the
// whole-compound verdict and the per-segment decomposition can never drift
// apart. A segment that is read-only-safe, a non-mutating cd whose target is
// provably the current directory, or a mutating command whose path args all
// stay in-root is VerdictAllow; anything the heuristic cannot prove safe is
// VerdictAsk. The deny verdict is reserved for the rules layer (isDangerous /
// a deny pattern), evaluated separately by evalRulesDetail before this is
// consulted — a segment that is not catastrophically dangerous but is otherwise
// unprovable is an ask, not a deny, exactly as classifyHeuristic treats it.
func segmentHeuristicVerdict(seg Segment, roots []string) (Verdict, string) {
	if isEgress(seg) && !isLoopbackFetch(seg, roots) {
		return VerdictAsk, LayerHeuristic + ": egress"
	}
	// A redirect target is a write no matter how read-only the command in
	// front of it is (`echo x > /etc/passwd`) and no matter which of the
	// exemptions below the segment qualifies for — the kill family's operands
	// are PIDs, and a loopback fetch's operands are URLs, but neither makes
	// their `> file` anything other than a file write. Scoping the targets
	// FIRST is therefore load-bearing: every `continue`/allow after this point
	// would carry them past unscoped (change 0070 D4).
	for _, target := range seg.WriteTargets {
		if !withinAnyRoot(target, roots) {
			return VerdictAsk, LayerHeuristic + ": out-of-root write target"
		}
	}
	if isReadOnlySafe(seg) {
		return VerdictAllow, ""
	}

	// Composition gate (compound0142 design item 2): a shell fed by a pipe
	// (cmd | sh, cmd | bash) and a shell whose script is an executed command
	// substitution (sh -c "$(cmd)", bash -c "$(cmd)") EXECUTE DATA. cat, echo,
	// git log and the rest are read-only with ANY arguments — including the
	// argument that is the whole payload — so the safelist would otherwise
	// admit them with no human, and `cat script | sh` must not inherit the
	// producers' allow. sh/bash are never read-only themselves, but shellparse
	// peels a parseable -c script into its inner segments and DISCARDS the
	// wrapper, so `bash -c "$(cat script)"` reaches here as [cat, script-body
	// segments] with no sh/bash segment left to fail on. The gate therefore
	// carries the peel as one segment named "bash" (Raw = the whole outer
	// `bash -c "…"` call, change 0093): an opaque -c script — a value that
	// cannot be statically proven safe — is unprovable ⇒ ask, and the
	// substitution's own inner segments surface alongside it for the deny and
	// egress layers. A LITERAL -c script (bash -c "git status") is always
	// peelable, so it keeps its pre-change decomposition and never trips this
	// branch, and a piped producer's read-only segments are judged on their own
	// zero-risk merits — the ask names the shell that runs them.
	if (seg.Name == "sh" || seg.Name == "bash") && seg.hasOpaqueArg() && strings.Contains(seg.Raw, "-c") {
		return VerdictAsk, LayerHeuristic + ": composition executes data"
	}

	// cd is a non-mutating command (change 0142): it changes the shell's
	// logical cwd but does not read, write, create, or delete any filesystem
	// object. The gate resolves every segment's path args against the ORIGINAL
	// allowed roots regardless of any preceding cd — and that is precisely why
	// a cd whose target is not statically known must NOT be waved through: the
	// shell resolves later relative paths against wherever the cd landed, so
	// once the target is not provably the current directory, every later
	// relative path is unproven (the hole this exemption closes). Exactly
	// qualify, and only qualify:
	//
	//   - The current-directory literals `$(pwd)`, `"$(pwd)"`, `$PWD`, `"$PWD"`:
	//     changes nothing, so it stays a deterministic allow. Identified
	//     EXACTLY as one argument whose text is one of those four strings —
	//     near-misses with a suffix (`"$(pwd)/.."`, `"$PWD/.."`) or a
	//     re-resolved path (`"$(cd /; pwd)"`) carry a different source text
	//     and fall to the ask below. The `$(pwd)` substitution's own `pwd`
	//     segment is read-only-safe and always allows; the exemption attaches
	//     to the cd.
	//   - A LITERAL target: proven in-root or out-of-root by path scoping
	//     exactly as before — `cd <root>` and `cd .` allow, `cd /etc` asks —
	//     and only an unknown target poisons later relative paths.
	//   - NO ARGS (bare `cd`): the shell goes to $HOME, which no segment can
	//     prove in-root. It asks when any segment follows; a lone bare cd (
	//     read-only around it) is allowed because nothing later needs the
	//     path proof.
	//   - `-` (`cd -`): the previous directory, likewise unprovable; asks
	//     when any segment follows.
	//   - AN OPAQUE target, an env prefix, or MORE THAN ONE argument: asks when
	//     any segment follows (safety wins over the routine-compound
	//     convenience). classifyHeuristic already asks the whole compound via
	//     the laundering check; this per-segment verdict surfaces the cd as
	//     the segment named in the audit/classifier decomposition.
	if seg.Name == "cd" {
		if isCdCurrentDir(seg) {
			return VerdictAllow, ""
		}
		// A bare cd (`cd` → $HOME) or `cd -` (previous dir): unprovable
		// targets with no path args to scope. Each alone is allowed — nothing
		// depends on the path it lands on, and cd is non-mutating. Whether
		// they LAUNDER later relative paths is a compound property: the
		// laundering check in classifyHeuristic and the segmentVerdicts
		// override (which knows the segment index) ask them when any non
		// carved-out segment follows.
		if len(seg.Args) == 0 {
			return VerdictAllow, ""
		}
		if len(seg.Args) == 1 && seg.Args[0] == "-" && !seg.hasOpaqueArg() {
			return VerdictAllow, ""
		}
		// More than one argument: the target meaning is not the plain-word
		// reading a single path arg would have (`cd -P "$HOME"` carries a mode
		// flag, `cd x y` is not a path cd) — unprovable ⇒ ask.
		if len(seg.Args) > 1 {
			return VerdictAsk, LayerHeuristic + ": cd to an unknown target (the paths after it are unproven)"
		}
		for _, arg := range pathArgs(seg) {
			if !withinAnyRoot(arg, roots) {
				return VerdictAsk, LayerHeuristic + ": cd out-of-root"
			}
		}
		if !seg.hasOpaqueArg() {
			// A single literal target that resolves in-root: historical
			// behaviour (`cd <root>`, `cd .`).
			return VerdictAllow, ""
		}
		return VerdictAsk, LayerHeuristic + ": cd to an unknown target (the paths after it are unproven)"
	}

	// THE INVARIANT of change 0070 D5, and the single most important line in
	// it. This segment MUTATES and carries at least one opaque argument — a
	// word whose value is not statically known ($VAR, $(…)). There is
	// nothing to prove containment over, so the answer is the human.
	//
	// Read this together with pathArgs, which drops opaque positions so an
	// opaque token can never reach withinWorkspace/resolveExisting. THAT
	// GUARD ALONE IS A WORSE BYPASS THAN NEITHER: with the token dropped,
	// `rm $(echo /)` arrives here with ZERO path args, the scoping loop
	// below finds nothing to fail on, and the function returns
	// VerdictAllow — auto-approving `rm /`. The drop removes the false
	// PROOF; this line supplies the missing verdict.
	//
	// It sits ahead of the kill and loopback-fetch exemptions on purpose:
	// both of those are proofs about operands ("these are numeric PIDs",
	// "these are loopback URLs"), and an operand we could not resolve is not
	// an operand either of them may claim to have inspected.
	if seg.hasOpaqueArg() {
		return VerdictAsk, LayerHeuristic + ": opaque argument"
	}
	// The kill family's operands are PIDs or process names, not paths —
	// path-scoping them against the cwd would prove nothing (change 0068).
	// A provably benign kill (numeric PIDs, signal flags only) allows;
	// everything else in the family routes to the classifier.
	switch seg.Name {
	case "kill":
		if isProvablyBenignKill(seg) {
			return VerdictAllow, ""
		}
		return VerdictAsk, LayerHeuristic + ": kill"
	case "pkill", "killall":
		return VerdictAsk, LayerHeuristic + ": " + seg.Name
	case "docker":
		// The read-only docker forms were admitted by the isReadOnlySafe
		// continue above; everything else docker reaches here. Its operands
		// are images, containers, services, and volumes — NAMES, not paths
		// (change 0072, same lesson as the kill family). Path-scoping them
		// would resolve "compose"/"up"/"data" as cwd-relative words that
		// trivially prove in-workspace, deterministically allowing
		// `docker volume rm data` or `docker system prune -f` with no
		// human. Nothing mutating may pass this layer; the classifier owns
		// docker's gray area.
		return VerdictAsk, LayerHeuristic + ": docker"
	case "find":
		// A read-only find (no -exec/-execdir/-ok/-okdir/-delete, no opaque
		// operand) was admitted by the isReadOnlySafe continue above. Any
		// find that reaches here carries a mutating action — and its
		// operands are PREDICATES, not paths: a `-exec rm -rf {}` argument
		// list is a command find runs, and path-scoping the surrounding
		// words ("." is always in-root!) proves nothing about that command.
		// `find . -name x -exec rm -rf {} ;` must never deterministic-allow
		// just because "." resolves under the root (Defect 2).
		return VerdictAsk, LayerHeuristic + ": find mutating action"
	}
	if isLoopbackFetch(seg, roots) {
		return VerdictAllow, ""
	}
	for _, arg := range pathArgs(seg) {
		if !withinAnyRoot(arg, roots) {
			return VerdictAsk, LayerHeuristic + ": out-of-root path"
		}
	}
	return VerdictAllow, ""
}

// isCdCurrentDir reports whether a cd segment changes nothing: exactly one
// argument whose parsed source text is one of the four current-directory
// forms — "$(pwd)", "$PWD", or their double-quoted spellings. Any other
// spelling (a suffix, a different expansion, "cd ." — whose literal is already
// proven by path scoping) is not this exemption, and opaqueness is irrelevant:
// the identity wins on the raw source text alone, which is what shellparse
// stores for an opaque word.
func isCdCurrentDir(seg Segment) bool {
	if len(seg.Args) != 1 {
		return false
	}
	switch seg.Args[0] {
	case `$(pwd)`, `"$(pwd)"`, `$PWD`, `"$PWD"`:
		return true
	}
	return false
}

// withinAnyRoot reports whether arg, resolved symlink-aware, stays within any
// of the allowed roots (change 0068). The empty-roots case is never within.
func withinAnyRoot(arg string, roots []string) bool {
	for _, root := range roots {
		if root != "" && withinWorkspace(arg, root) {
			return true
		}
	}
	return false
}

// protectedInRoot reports whether path, once resolved, sits in a part of an allowed
// root that a later process trusts: .git (hooks and config run on the host, outside
// any sandbox) and fuse's own .fuse and .fuse.local.yml. A write there can outlive
// this run, so it needs a person even though it is inside the workspace. An
// unresolvable path counts as protected, failing toward the human.
func protectedInRoot(path string, roots []string) bool {
	resolved, ok := resolveForWrite(path)
	if !ok {
		return true
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		base, err := filepath.EvalSymlinks(root)
		if err != nil {
			base = root
		}
		rel, err := filepath.Rel(base, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if seg == ".git" || seg == ".fuse" || seg == ".fuse.local.yml" {
				return true
			}
		}
	}
	return false
}

// resolveForWrite resolves symlinks in path, walking up to the nearest existing
// ancestor so a file that does not exist yet still resolves through its parents.
func resolveForWrite(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest), true
		}
		if dir == filepath.Dir(dir) {
			return "", false
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// egressNames are argv[0] basenames that open a network connection to an
// arbitrary host. Their egress is a hard risk boundary regardless of arguments.
var egressNames = map[string]bool{
	"curl":  true,
	"wget":  true,
	"nc":    true,
	"ncat":  true,
	"ssh":   true,
	"scp":   true,
	"sftp":  true,
	"rsync": true,
}

// gitEgressSubcommands are git subcommands that reach a remote when handed a
// host-qualified URL argument (as opposed to a local remote name).
var gitEgressSubcommands = map[string]bool{
	"clone": true,
	"fetch": true,
	"pull":  true,
	"push":  true,
}

// gitGlobals is the set of single-word git global options that neither run a
// command nor move the repository, so they can be stepped over freely.
var gitGlobals = map[string]bool{
	"--no-pager":           true,
	"-P":                   true,
	"--no-optional-locks":  true,
	"--no-replace-objects": true,
	"--literal-pathspecs":  true,
}

// gitAfterGlobals returns the git argument list with every LEADING `-C <word>`
// and `-c <word>` pair stripped, and every leading single word from gitGlobals,
// stopping at the first other word. git runs its subcommand after those
// globals, so the subcommand and its operands sit at the end of that walk; the
// egress checks below read the view rather than the raw list so
// `git -C sub push` is seen as a push and `git -C /outside push` as well.
//
// Unlike isSafeGit's -C handling this makes no claim about the VALUE: whether
// the directory is inside or outside the workspace decides where a read lands,
// not whether a push is a push. The egress boundary is the operation, and a
// `-C /outside` that escaped the read-only allowance is exactly the shape that
// must still be recognised here.
func gitAfterGlobals(args []string) []string {
	i := 0
	for i < len(args) {
		if args[i] == "-C" || args[i] == "-c" {
			if i+1 >= len(args) {
				break
			}
			i += 2
			continue
		}
		if gitGlobals[args[i]] {
			i++
			continue
		}
		break
	}
	return args[i:]
}

// isEgress reports whether a segment performs network egress: a known egress
// tool, or a git remote operation given a host-qualified URL. `git push` is
// egress UNCONDITIONALLY (change 0068): its usual operands (`origin`, a branch
// name) resolve under the cwd and would otherwise slip through the mutating
// path scope as in-workspace, silently allowing a publish. clone/fetch/pull
// keep the host-qualified requirement (a bare `git pull` against a configured
// remote is routine sync, and its worst case pulls INTO the workspace).
func isEgress(seg Segment) bool {
	if egressNames[seg.Name] {
		return true
	}
	if isGitPush(seg) {
		return true
	}
	if seg.Name == "git" && len(seg.Args) > 0 {
		view := gitAfterGlobals(seg.Args)
		// Offset back into the raw Args/Opaque slices: isOpaque is indexed
		// against the ORIGINAL argument list, so every index the view yields
		// must be shifted by the pairs stripped above to stay a correct
		// opacity answer rather than an off-by-two read of the wrong word.
		off := len(seg.Args) - len(view)
		if len(view) > 0 && !seg.isOpaque(off) && gitEgressSubcommands[seg.Args[off]] {
			for i, a := range view[1:] {
				// An OPAQUE remote argument counts as host-qualified (change 0070
				// D5): isHostQualified inspects the text for "://", "@" and ":"
				// markers, and "$REMOTE" carries none of them — so `git clone $URL`
				// would read as a LOCAL remote name and skip the egress boundary
				// entirely. Unprovable means we take the stricter reading.
				if seg.isOpaque(off+i+1) || isHostQualified(a) {
					return true
				}
			}
		}
	}
	return false
}

// isLoopbackHost reports whether host is the loopback interface: the literal
// "localhost" or an IP with IsLoopback. Deliberately narrower than the
// web_fetch SSRF check (no RFC-1918/link-local): only the machine's own
// services qualify for the deterministic curl allow (change 0068).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// curl/wget flag tables for isLoopbackFetch. A flag outside these tables makes
// the segment unprovable (⇒ not a loopback fetch ⇒ egress ask ⇒ classifier):
// curl has many flags that write files (-c, -D, --trace, -K …) and enumerating
// the benign set is the only fail-closed posture.
var (
	curlFlagsNoValue = map[string]bool{
		"-s": true, "--silent": true, "-S": true, "--show-error": true,
		"-f": true, "--fail": true, "-L": true, "--location": true,
		"-v": true, "--verbose": true, "-i": true, "--include": true,
		"-I": true, "--head": true, "-k": true, "--insecure": true,
		"-4": true, "-6": true, "-N": true, "--no-buffer": true,
		"-g": true, "--globoff": true, "--compressed": true,
		"-O": true, "--remote-name": true, // writes basename into the cwd (the workspace)
	}
	curlFlagsWithValue = map[string]bool{
		"-X": true, "--request": true, "-H": true, "--header": true,
		"-d": true, "--data": true, "--data-raw": true, "--data-binary": true,
		"-m": true, "--max-time": true, "--connect-timeout": true,
		"-w": true, "--write-out": true, "-u": true, "--user": true,
		"--retry": true, "-A": true, "--user-agent": true,
	}
	curlFlagsWithScopedValue = map[string]bool{
		"-o": true, "--output": true,
	}
	wgetFlagsNoValue = map[string]bool{
		"-q": true, "--quiet": true, "-nv": true, "--no-verbose": true,
		"-4": true, "-6": true, "--no-check-certificate": true,
	}
	wgetFlagsWithValue = map[string]bool{
		"-T": true, "--timeout": true, "-t": true, "--tries": true,
		"-U": true, "--user-agent": true, "--header": true,
		"--post-data": true, "--method": true,
	}
	wgetFlagsWithScopedValue = map[string]bool{
		"-O": true, "--output-document": true, "-P": true, "--directory-prefix": true,
	}
)

// isLoopbackFetch reports whether a curl/wget segment is provably a loopback
// fetch (change 0068): every URL-shaped operand's host is loopback, every
// output-writing flag's target stays within the allowed roots, and every flag
// is on the known-benign table. Anything unprovable — a non-loopback or
// non-literal URL, an unknown flag, a missing flag value — returns false and
// the segment keeps its egress-ask ⇒ classifier routing.
func isLoopbackFetch(seg Segment, roots []string) bool {
	// An OPAQUE operand anywhere sinks the proof (change 0070 D5). This whole
	// function is an argument-by-argument PROOF, and every arm of the loop below
	// reads a word's text as a value — as a known flag, as a flag's scoped
	// output path, or as a URL whose host must be loopback. An opaque word is
	// none of those: `curl $URL` fetches an arbitrary host, `curl -o $OUT …`
	// writes an arbitrary file, and urlHost would cheerfully parse a "hostname"
	// out of a token that is not a URL at all. `curl $URL` must not inherit the
	// loopback allow.
	if seg.hasOpaqueArg() {
		return false
	}
	var noVal, withVal, scopedVal map[string]bool
	switch seg.Name {
	case "curl":
		noVal, withVal, scopedVal = curlFlagsNoValue, curlFlagsWithValue, curlFlagsWithScopedValue
	case "wget":
		noVal, withVal, scopedVal = wgetFlagsNoValue, wgetFlagsWithValue, wgetFlagsWithScopedValue
	default:
		return false
	}
	sawURL := false
	for i := 0; i < len(seg.Args); i++ {
		a := seg.Args[i]
		switch {
		case a == "":
			continue
		case noVal[a]:
			continue
		case withVal[a]:
			i++ // consume the value
			if i >= len(seg.Args) {
				return false
			}
		case scopedVal[a]:
			i++
			if i >= len(seg.Args) || !withinAnyRoot(seg.Args[i], roots) {
				return false
			}
		case strings.HasPrefix(a, "--"):
			return false // unknown long flag ⇒ unprovable
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A grouped short form (curl -sL): provable only when every letter is
			// itself a known no-value short flag.
			if !allShortFlagsNoValue(a[1:], noVal) {
				return false
			}
		case strings.HasPrefix(a, "-"):
			return false // bare "-" ⇒ unprovable
		default:
			host, ok := urlHost(a)
			if !ok || !isLoopbackHost(host) {
				return false
			}
			sawURL = true
		}
	}
	return sawURL
}

// allShortFlagsNoValue reports whether every letter of a grouped short flag is
// individually a known no-value flag ("-sL" ⇒ "-s" and "-L").
func allShortFlagsNoValue(letters string, noVal map[string]bool) bool {
	for _, r := range letters {
		if !noVal["-"+string(r)] {
			return false
		}
	}
	return true
}

// urlHost extracts the hostname from a URL-shaped operand, accepting both
// scheme URLs (http://localhost:4000/x) and the schemeless host[:port][/path]
// form curl accepts (localhost:4000/v1/models). ok is false for anything that
// does not parse to a non-empty host.
func urlHost(arg string) (string, bool) {
	s := arg
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	// Only http(s) shapes qualify; anything else (file://, ftp://, gopher://)
	// is not the loopback health-check shape.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	return u.Hostname(), true
}

// isHostQualified reports whether a git remote argument names a host rather than
// a local remote name — a scheme URL (https://…, git://…, ssh://…) or an
// scp-style host:path / user@host:path. A bare token with no host marker is a
// local remote name (origin) and is not egress by itself.
func isHostQualified(arg string) bool {
	if strings.HasPrefix(arg, "-") {
		return false // a flag, not a remote.
	}
	if strings.Contains(arg, "://") {
		return true
	}
	if strings.Contains(arg, "@") && strings.Contains(arg, ":") {
		return true // user@host:path
	}
	// scp-style host:path — a colon before any slash, with a non-empty host.
	if i := strings.IndexByte(arg, ':'); i > 0 {
		slash := strings.IndexByte(arg, '/')
		if slash == -1 || i < slash {
			return true
		}
	}
	return false
}

// pathArgs returns the non-flag operands of a mutating segment that are treated
// as filesystem paths to be scoped. Flags (leading '-') and their nature are
// not inspected individually; instead every non-flag word is conservatively
// scoped — a word that is not actually a path but happens to resolve outside the
// workspace only costs a human prompt, never a bypass.
//
// OPAQUE POSITIONS ARE SKIPPED (change 0070 D5), which is the enforcement point
// of this change's invariant. An opaque arg's stored text is the raw source of
// the word ("$VAR", "$(echo /)"), not a path, and every caller of this function
// hands its result to withinWorkspace — which resolves a relative operand
// against the cwd and walks up to the deepest EXISTING ancestor. Emitting
// "$(echo /)" would therefore resolve it to <cwd>/$(echo /), whose parent IS the
// workspace, and PROVE containment for a command that deletes the filesystem
// root. Same failure family as #0068's unexpanded `~` and process-name
// operands: a containment proof is sound only over an operand that is a path.
//
// It iterates by INDEX rather than `range seg.Args` by value precisely so the
// opaqueness flag — which lives in a parallel slice — is still reachable.
//
// Skipping here is only half the answer: dropping an operand also removes the
// thing the caller would have failed on. classifyHeuristic supplies the other
// half by asking outright on any mutating segment with an opaque arg. Neither
// guard is safe without the other.
func pathArgs(seg Segment) []string {
	var out []string
	for i, a := range seg.Args {
		if a == "" {
			continue
		}
		if seg.isOpaque(i) {
			continue // unprovable ⇒ never resolved as a path. See above.
		}
		if strings.HasPrefix(a, "-") {
			continue // an option flag, not a path operand.
		}
		// dd's operands are key=value (of=/tmp/x, if=in.img): scope the value's
		// path, not the literal "of=/tmp/x" token — which would resolve as a
		// weird cwd-relative filename and wrongly prove containment (0068).
		if seg.Name == "dd" {
			if v, ok := strings.CutPrefix(a, "of="); ok {
				out = append(out, v)
				continue
			}
			if v, ok := strings.CutPrefix(a, "if="); ok {
				out = append(out, v)
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// withinWorkspace reports whether arg, resolved symlink-aware, stays within
// workspaceRoot. It resolves the deepest existing ancestor when the leaf does
// not yet exist (a file about to be created), so a to-be-written path scopes on
// its real parent directory rather than a lexical guess.
//
// This never trusts a lexical prefix of the pre-resolution path: an in-workspace
// symlink whose target escapes the root is caught because the link itself is
// resolved (dirent-isdir-skips-symlinks). A relative arg resolves against the
// current working directory (the caller's process cwd == the workspace).
func withinWorkspace(arg, workspaceRoot string) bool {
	// Tilde reaches us as a literal (the parser does not expand it) but bash
	// WILL expand it at execution: ~/x is a home-anchored path, not a cwd file
	// named "~". Expand our own home; a ~user form for another user is
	// unprovable ⇒ fail toward the human (change 0068).
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		arg = home + arg[1:]
	} else if strings.HasPrefix(arg, "~") {
		return false
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return false // cannot resolve ⇒ fail toward the human.
	}

	resolved, ok := resolveExisting(abs)
	if !ok {
		return false
	}
	return isWithin(resolved, workspaceRoot)
}

// resolveExisting resolves p with EvalSymlinks. When p's leaf does not exist
// yet, it resolves the deepest existing ancestor and re-appends the missing
// trailing components, so a not-yet-created leaf still scopes on its real
// parent. ok is false if not even an ancestor can be resolved.
func resolveExisting(p string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, true
	}
	// The leaf (or deeper) does not exist. Walk up to the deepest ancestor that
	// does, resolve it, then re-append the missing tail.
	dir := p
	var missing []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // reached the root without finding an existing ancestor.
		}
		missing = append([]string{filepath.Base(dir)}, missing...)
		dir = parent
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...), true
		}
	}
}

// isWithin reports whether path is workspaceRoot itself or a descendant of it,
// using a cleaned, separator-boundary-aware prefix on the resolved paths (both
// already symlink-resolved by the caller).
func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// Any rel that steps up out of root begins with "..".
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
