package permissions

// denyHint returns actionable guidance appended to a policy-denial result
// (change 0067). Models retry denied calls verbatim — the hint tells them why
// that will keep failing and what to do instead, which is what breaks the
// deny→identical-retry amplification before the loop-level nudge has to.
func denyHint(layer string) string {
	switch layer {
	case LayerRules:
		return "this command is blocked by auto-mode policy as catastrophic; retrying the identical call will fail — change the command or ask the user"
	case LayerHeuristic:
		return "the command touches paths or network targets outside the allowed scope; write inside the workspace root, or use web_fetch/web_search for network reads"
	case LayerEditScope:
		return "the target path is outside the workspace; write inside the workspace root instead"
	case LayerFetchFloor:
		return "this host is blocked by the web_fetch host floor; fetching it will never be approved — use a different source"
	case LayerClassifier:
		return "the auto-mode classifier judged this call unsafe; retrying the identical call will fail — change approach or ask the user"
	case LayerValve:
		return "auto mode is paused; further gray-area calls will not be auto-approved this session"
	case LayerMediation:
		return "the target is outside this principal's allowlist; no retry will succeed"
	default:
		return "retrying the identical call will fail — change the command or ask the user"
	}
}
