package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// HandlerContainer is the bounded handler identifier for the container
// substrate, and the default this loader resolves to.
//
// It is declared here rather than alongside the container handler because the
// config loader must be able to name the default substrate whether or not a
// container handler is built; the constant is the closed enum value, not the
// implementation.
const HandlerContainer = "container"

const (
	// configDirName and configFileName locate the operator's off-switch file
	// relative to the repo root. The ".local." infix follows this repo's
	// existing trusted-local, machine-scoped file posture: the file is
	// gitignored and never travels with the code, because authorizing
	// uncontained execution is a property of one operator's machine, never of
	// the project.
	configDirName  = ".fuse"
	configFileName = "sandbox.local.yml"
)

// DefaultIdleTTL is how long a warm Runner may sit idle before the pool reaper
// tears it down. It is a backstop against leaked substrate, not a scheduling
// knob, so it is deliberately generous.
//
// It is also the value every degraded config path falls back to. That matters:
// a zero TTL would mean "reap immediately", so a malformed value must never be
// allowed to become the zero value by omission.
const DefaultIdleTTL = 5 * time.Minute

// Defaults for the pull timeout and the admission gate, applied to fields the operator
// left unset. Unset resource caps stay unset: no cap flags are emitted, since capping a
// developer's own build defends the machine from its operator against no threat model.
//
// The numbers are each overridable in one line of the trusted-local config.
const (
	// DefaultPullTimeout bounds the explicit pre-pull.
	DefaultPullTimeout = 2 * time.Minute

	// Admission gate defaults.
	defaultMaxInflight          int64 = 64
	defaultMaxInflightPerTenant int64 = 16
	defaultMaxQueued            int64 = 256
	// DefaultNoteThreshold is the wait at or above which the model-visible
	// backpressure note is attached.
	DefaultNoteThreshold = 2 * time.Second
)

// resolveDefaults fills the unset pull timeout and Concurrency fields on cfg with their
// defaults, in place. It is idempotent and only ever fills NIL fields. It is called by
// NewService.
func (cfg *Config) resolveDefaults() {
	l := &cfg.Limits
	if l.PullTimeout == nil {
		v := DefaultPullTimeout
		l.PullTimeout = &v
	}

	c := &cfg.Concurrency
	if c.MaxInflight == nil {
		v := defaultMaxInflight
		c.MaxInflight = &v
	}
	if c.MaxInflightPerTenant == nil {
		v := defaultMaxInflightPerTenant
		c.MaxInflightPerTenant = &v
	}
	if c.MaxQueued == nil {
		v := defaultMaxQueued
		c.MaxQueued = &v
	}
	if c.NoteThreshold == nil {
		v := DefaultNoteThreshold
		c.NoteThreshold = &v
	}

}

// Config is the resolved sandbox configuration.
//
// A Config obtained from LoadConfig or DefaultConfig is always internally
// consistent and always safe to act on directly: there is no "unset" state and
// no error state a caller must remember to check before using it. Every path
// that could not produce a trustworthy answer produces the contained default
// instead — plus, for egress alone, the salvaged posture, because there the
// contained default is not the restrictive answer.
type Config struct {
	// Contained reports whether the command must run isolated from this host.
	// It is the boolean shadow of Handler and is maintained in lockstep with
	// it: Contained is true exactly when Handler is not HandlerHost. Two
	// callers reading different fields can therefore never reach different
	// conclusions about containment.
	Contained bool

	// Handler is the authoritative bounded substrate identifier — one of
	// HandlerContainer or HandlerHost, never free-form text from the file.
	Handler string

	// Image is the container image reference, honoured by the container
	// handler only. Empty means the handler's built-in default.
	Image string

	// EnvPassthrough is the operator's declared list of additional environment
	// variable NAMES that sandboxed commands may observe, on top of the base
	// allowlist. It carries names only; values are resolved from the host at
	// Exec time by ResolveEnv. Blank entries are dropped and declaration order
	// is preserved.
	EnvPassthrough []string

	// IdleTTL is the warm-pool reaper's idle backstop. It is always positive.
	IdleTTL time.Duration

	// Limits are the per-container cgroup caps (container handler only). Every
	// field is OPTIONAL and carries its own presence: an unset field emits no
	// flag, and an explicit zero is distinguishable from absent. LoadConfig fills
	// only what the operator wrote.
	Limits Limits

	// Concurrency is the admission gate's configuration — the concurrency
	// backstop and the pull timeout. A queue that never refuses under ordinary
	// load costs a laptop nothing.
	Concurrency Concurrency

	// Egress is the container's network posture (change 0064). Its zero value
	// is allow-all — default container networking.
	//
	// Egress posture is selected by the EXPLICIT egress.mode knob only. It is
	// never derived from model output.
	Egress Egress
}

// EgressMode is the bounded egress posture selector. It is a closed enum, and
// its ZERO VALUE is deliberately the permissive one: an unconfigured Config must
// keep default container networking so local development is unchanged. Every
// path that *fails* rather than being unconfigured resolves to EgressEnforce
// with an empty allowlist instead — see rawEgress.resolve for a parseable file
// and salvageEgressPosture for one discarded wholesale.
type EgressMode int

const (
	// EgressAllowAll is the default: no floor, no proxy, default container
	// networking. It is the local-dev experience and the containment default.
	EgressAllowAll EgressMode = iota
	// EgressEnforce turns on the --network none floor. Nothing reaches the network.
	EgressEnforce
)

// String renders the mode with its config spelling, so a diagnostic naming the
// mode names something the operator can type back into the file.
func (m EgressMode) String() string {
	switch m {
	case EgressAllowAll:
		return "allow-all"
	case EgressEnforce:
		return "enforce"
	default:
		return "unknown(" + strconv.Itoa(int(m)) + ")"
	}
}

// Egress is the resolved egress posture.
type Egress struct {
	// Mode is the posture.
	Mode EgressMode
}

// Limits are the per-container cgroup caps. Each field is a pointer so "unset"
// (the operator said nothing) is distinguishable from an explicit zero (the
// operator asked for no cap): the first resolves to a posture default, the
// second emits no flag. Bytes fields are resolved to an integer count and
// rendered by the argv builder; CPUs is a decimal string rendered verbatim.
type Limits struct {
	// MemoryBytes caps --memory (with --memory-swap pinned equal). nil ⇒ unset.
	MemoryBytes *int64
	// CPUs caps --cpus, as a decimal string ("2.0"). nil ⇒ unset. It is carried
	// as a string, not a float, so argv rendering is deterministic and
	// locale-independent — argv is golden-tested.
	CPUs *string
	// Pids caps --pids-limit. nil ⇒ unset.
	Pids *int64
	// NoFile caps --ulimit nofile=N:N. nil ⇒ unset.
	NoFile *int64
	// FsizeBytes caps --ulimit fsize=<bytes>. It bounds ONE file, not the mount;
	// a real per-tenant disk quota is #0065's filesystem work. nil ⇒ unset.
	FsizeBytes *int64
	// PullTimeout bounds the explicit pre-pull. nil ⇒ unset; it is always
	// resolved to a positive default (in both postures) by NewService.
	PullTimeout *time.Duration
}

// Concurrency configures the admission gate. Every field is a pointer so unset
// is distinguishable from an explicit zero, and NewService fills every unset
// field with a default (the same default in both postures).
type Concurrency struct {
	// MaxInflight is the GLOBAL runaway backstop — the aggregate ceiling on
	// concurrent in-flight Execs across every tenant on this host. Chosen high
	// enough that ordinary load never touches it. nil ⇒ unset.
	MaxInflight *int64
	// MaxInflightPerTenant is one tenant's soft share of the global budget, so a
	// single tenant's burst queues against its own share and other tenants keep
	// flowing. nil ⇒ unset.
	MaxInflightPerTenant *int64
	// MaxQueued is the WAITER overflow bound — the pathological-case refusal. A
	// caller is refused only when max_inflight Execs are running AND max_queued
	// more are already parked behind them. nil ⇒ unset.
	MaxQueued *int64
	// NoteThreshold is the wait at or above which the model-visible backpressure
	// note is attached. A very large value silences the note without touching the
	// gate. nil ⇒ unset.
	NoteThreshold *time.Duration
}

// DefaultConfig is the fail-safe configuration: contained, on the container
// substrate, with no operator passthrough and the default reaper backstop.
//
// It is what every absent, unreadable, malformed, or un-understood config
// resolves to. Nothing about it is derived from the environment, from a wire
// field, or from model output.
func DefaultConfig() Config {
	return Config{
		Contained: true,
		Handler:   HandlerContainer,
		IdleTTL:   DefaultIdleTTL,
	}
}

// WarnReason is the bounded reason a config load was degraded. It is a closed
// enum so it can be used as an event or metric label without unbounded
// cardinality, and so a caller can branch on it without string matching.
type WarnReason string

const (
	// WarnNoRoot means no repo root was supplied, so no file was consulted.
	WarnNoRoot WarnReason = "no_root"
	// WarnUnreadable means the file exists but could not be read.
	WarnUnreadable WarnReason = "unreadable"
	// WarnMalformed means the file could not be parsed as the expected shape,
	// including an unrecognised key.
	WarnMalformed WarnReason = "malformed"
	// WarnUnknownHandler means handler: named a substrate that does not exist.
	WarnUnknownHandler WarnReason = "unknown_handler"
	// WarnContradictory means contained: and handler: disagreed.
	WarnContradictory WarnReason = "contradictory"
	// WarnBadIdleTTL means pool.idle_ttl was unparsable or non-positive.
	WarnBadIdleTTL WarnReason = "bad_idle_ttl"
	// WarnBadLimit means a limits.* value was unparsable or non-positive. The
	// offending field degrades to unset (a posture default), never to a zero cap.
	WarnBadLimit WarnReason = "bad_limit"
	// WarnBadConcurrency means a concurrency.* value was unparsable or
	// non-positive. The offending field degrades to unset (a built-in default),
	// never to a zero bound — a zero backstop would refuse every Exec.
	WarnBadConcurrency WarnReason = "bad_concurrency"
	// WarnUnknownEgressMode means egress.mode named a posture that does not
	// exist. The posture resolves to enforcement with an EMPTY allowlist: an
	// unparseable posture must never resolve to the permissive one.
	WarnUnknownEgressMode WarnReason = "unknown_egress_mode"
	// WarnBadEgress means the file declares an egress.allow list. fuse has no egress
	// proxy to honour one, so the list is ignored: enforce stays deny-all.
	WarnBadEgress WarnReason = "bad_egress"
)

// Warning is a LOUD but non-fatal diagnostic from a config load.
//
// It is deliberately NOT returned as an error. An error return invites a caller
// to write `if err != nil { /* no usable config, carry on */ }`, and "carry on"
// at this particular boundary means running model-authored shell commands
// uncontained. Because a Warning is not an error, there is no failure mode in
// which mishandling it disables containment: the Config returned alongside it
// is always already the safe answer, and the Warning exists purely so the
// operator learns that their file did not do what they thought it did.
//
// The composition root MUST log every Warning it receives at warning level or
// above. Silently discarding one leaves an operator believing a config is in
// force when it is not.
type Warning struct {
	// Reason is the bounded cause.
	Reason WarnReason
	// Path is the config file the loader tried to use, when there was one.
	Path string
	// Detail is the underlying diagnostic (a parse error, a rejected value).
	// It is for humans and logs; never parse it.
	Detail string
	// Effect states, in plain words, what the loader did instead.
	Effect string
}

// Error renders the warning. Warning implements error so callers can hand it
// straight to a logger's error-shaped field, but LoadConfig never returns one
// in an error position.
func (w Warning) Error() string {
	path := w.Path
	if path == "" {
		path = configDirName + "/" + configFileName
	}
	msg := fmt.Sprintf("sandbox config [%s]: %s: %s", w.Reason, path, w.Effect)
	if w.Detail != "" {
		msg += " (" + w.Detail + ")"
	}
	return msg
}

// rawConfig is the on-disk shape. Every scalar is a pointer so the loader can
// tell "absent" from "explicitly set to the zero value" — the difference
// between an operator who said nothing and one who said `contained: false`.
type rawConfig struct {
	Contained      *bool           `yaml:"contained"`
	Handler        *string         `yaml:"handler"`
	Image          *string         `yaml:"image"`
	EnvPassthrough []string        `yaml:"env_passthrough"`
	Pool           *rawPool        `yaml:"pool"`
	Limits         *rawLimits      `yaml:"limits"`
	Concurrency    *rawConcurrency `yaml:"concurrency"`
	Egress         *rawEgress      `yaml:"egress"`
}

// rawEgress mirrors the egress: block. mode is a string so an unrecognised
// posture is REPORTED rather than coerced, exactly like handler:.
type rawEgress struct {
	Mode  *string `yaml:"mode"`
	Allow []any   `yaml:"allow"`
}

type rawPool struct {
	// IdleTTL is decoded as a string rather than a time.Duration so that a
	// value YAML happens to read as a number ("300") degrades to a warning
	// rather than to a silently different meaning.
	IdleTTL *string `yaml:"idle_ttl"`
}

// rawLimits mirrors the limits: block. Byte-valued caps decode as strings so
// human sizes ("2g", "512m") are accepted and a bare number degrades to a
// warning rather than a silently-different meaning. cpus is likewise a string —
// it is a decimal fraction, not an integer count. Every field is a pointer so
// absent is distinguishable from an explicit zero.
type rawLimits struct {
	Memory      *string `yaml:"memory"`
	CPUs        *string `yaml:"cpus"`
	Pids        *int64  `yaml:"pids"`
	NoFile      *int64  `yaml:"nofile"`
	Fsize       *string `yaml:"fsize"`
	PullTimeout *string `yaml:"pull_timeout"`
}

// rawConcurrency mirrors the concurrency: block. The counts are plain integers;
// note_threshold is a duration string, decoded like idle_ttl.
type rawConcurrency struct {
	MaxInflight          *int64  `yaml:"max_inflight"`
	MaxInflightPerTenant *int64  `yaml:"max_inflight_per_tenant"`
	MaxQueued            *int64  `yaml:"max_queued"`
	NoteThreshold        *string `yaml:"note_threshold"`
}

// LoadConfig reads the operator off-switch file at <root>/.fuse/sandbox.local.yml.
// The process passes the home directory as root, never the working tree.
//
// It returns the resolved Config and any non-fatal Warnings. It has no error
// return, by design (see Warning): there is no outcome in which a caller is
// handed something other than a directly usable, already-safe Config.
//
// FAIL-SAFE, NEVER FAIL-OPEN. Absent, unreadable, malformed, un-understood, and
// contradictory-toward-containment inputs all resolve to DefaultConfig(), which
// is contained. The ONLY way to reach the host substrate is a readable,
// well-formed file that explicitly says so.
//
// The one dimension for which DefaultConfig() is not the safe answer is EGRESS,
// whose default is the permissive posture. A file discarded wholesale therefore
// resolves to DefaultConfig() carrying the salvaged egress POSTURE — never a
// salvaged allowlist — so a mistyped key cannot revert enforcement to allow-all.
// See the "whole-file discard" block below.
//
// THE OFF-SWITCH IS FILE-ONLY. This function reads no environment variable, and
// must never be changed to. Containment is likewise never derived from a wire
// field, a tool argument, or model output; the file is a machine-local operator
// artifact, and the caller is responsible for resolving root to a trusted repo
// root and for calling this once at process or loop startup — not per command,
// where an agent that can write files could author its own off-switch.
//
// Note that reaching the host substrate additionally requires selection to
// permit it; this loader only reports what the operator asked for.
func LoadConfig(root string) (Config, []Warning) {
	if strings.TrimSpace(root) == "" {
		// Resolving against the process working directory would let whatever
		// .fuse/ happens to be under the agent's cwd decide containment. A
		// caller that did not supply a root gets the safe default instead.
		return DefaultConfig(), []Warning{{
			Reason: WarnNoRoot,
			Effect: "no repo root supplied, so no config file was read; running contained on the container substrate",
		}}
	}

	path := filepath.Join(root, configDirName, configFileName)

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No file is the overwhelmingly common case and is not a problem:
			// it is what every machine that never opted out looks like.
			return DefaultConfig(), nil
		}
		// Permission denied, an I/O error, a directory where a file belongs.
		// We do not know what the file says, so we must not guess — and the
		// only safe guess would be the default anyway.
		return DefaultConfig(), []Warning{{
			Reason: WarnUnreadable,
			Path:   path,
			Detail: err.Error(),
			Effect: "config file could not be read; running contained on the container substrate",
		}}
	}

	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// An unrecognised key is treated as a malformed file rather than silently
	// ignored. A key we do not understand is usually an operator configuring
	// something that is not happening, and finding that out loudly beats
	// finding it out never. The failure direction is contained either way.
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty file is indistinguishable in intent from an absent one.
			return DefaultConfig(), nil
		}
		// The file is discarded wholesale — except for the egress POSTURE, which
		// is the one dimension whose default is the permissive side. See
		// salvageEgressPosture.
		egress := salvageEgressPosture(data)
		return discardedConfig(egress), []Warning{{
			Reason: WarnMalformed,
			Path:   path,
			Detail: strings.TrimSpace(err.Error()),
			Effect: "config file could not be parsed; running contained on the container substrate" + egressEffect(egress),
		}}
	}

	return raw.resolve(path)
}

// --- whole-file discard: salvaging the egress posture -------------------------
//
// Change 0063 could discard a config file WHOLESALE and land on DefaultConfig()
// because the default was the SAFE side of every dimension the file carried:
// contained, container substrate, no image, no passthrough. Reverting to it
// could only ever narrow what a command could do.
//
// Change 0064 adds a dimension whose default is the UNSAFE side. Egress's zero
// value is EgressAllowAll — unrestricted, by design, so an unconfigured machine
// is unchanged. That inverts the discard rule for this one field: an operator
// who wrote `egress.mode: enforce` and then mistyped an unrelated key would have
// their enforcement silently reverted to allow-all, which is precisely the
// fail-OPEN direction the spec forbids ("fail toward deny-all, never toward the
// internet"). A loud warning does not change the resolved posture.
//
// So a whole-file discard salvages the POSTURE, and nothing else:
//
//   - The ALLOWLIST is never salvaged. An entry recovered from a file we could
//     not parse would be an authorization decision derived from bytes we do not
//     trust; deny-all is the correct outcome. Enforce-with-an-empty-allowlist is
//     already a legitimate, well-supported resolved state (see Egress).
//   - The salvage never INVENTS enforcement. A file that asked for allow-all, or
//     said nothing about egress, keeps the unchanged local-dev posture —
//     otherwise an unrelated typo would black out a developer's network.
//   - No OTHER dimension's discard behaviour changes. Containment, handler,
//     image, and env_passthrough keep exactly the semantics 0063 defined.
//
// The paths with no bytes to read (WarnNoRoot, WarnUnreadable) have nothing to
// salvage from and are deliberately untouched: there is no file content from
// which a posture could be derived, and inventing one is not an option.

// rawPosture is the deliberately minimal shape used for the second, permissive
// decode. It carries the posture selector and NOTHING else, so no field of a
// file we rejected can reach a Config through this door — not even by accident,
// because the struct has nowhere to put one.
type rawPosture struct {
	Egress *struct {
		Mode *string `yaml:"mode"`
	} `yaml:"egress"`
}

// salvageEgressPosture recovers the egress posture, and only the posture, from
// the bytes of a config file that could not be parsed as the expected shape.
func salvageEgressPosture(data []byte) Egress {
	// KnownFields is deliberately OFF here. The strict decode already ran and
	// already rejected the file; this pass is not a second chance at honouring
	// it, it is a targeted read of one enum whose default is unsafe. The
	// commonest reason to be here — a mistyped key elsewhere in the file — is
	// exactly the case a permissive decode sees through.
	var raw rawPosture
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err == nil {
		if raw.Egress == nil || raw.Egress.Mode == nil {
			return Egress{}
		}
		return salvagedMode(*raw.Egress.Mode)
	}
	// The document does not parse as YAML at all (an unterminated quote, a
	// broken flow sequence), so no decoder — however permissive — can reach the
	// mode. Fall back to reading the posture out of the text.
	return scanEgressPosture(data)
}

// salvagedMode maps a mode string recovered from a discarded file onto a
// posture, using the same no-fallback-toward-permissive rule as parseEgressMode:
// only a value that recognisably says allow-all resolves to allow-all. An
// unrecognised spelling resolves to enforcement, matching what a PARSEABLE file
// with the same typo would have resolved to (WarnUnknownEgressMode).
func salvagedMode(mode string) Egress {
	if m, ok := parseEgressMode(mode); ok && m == EgressAllowAll {
		return Egress{}
	}
	return Egress{Mode: EgressEnforce}
}

// scanEgressPosture is the last-resort posture read for a file that is not
// YAML. It looks for a `mode:` key line — `mode` appears nowhere else in this
// schema — and reads its value.
//
// This is a text scan, not a parser, and it is deliberately biased: every way it
// can be wrong about a `mode:` line's structure resolves toward ENFORCEMENT,
// whose cost is a loudly-warned deny-all on a file the operator must fix anyway.
// It is reached only for a file that has already failed two decoders, and it can
// never widen access, so its imprecision has no fail-open direction. It reads
// only the posture; it cannot and does not recover an allow entry.
func scanEgressPosture(data []byte) Egress {
	for _, line := range strings.Split(string(data), "\n") {
		// Strip comments first, so a commented-out `# mode: enforce` is
		// correctly read as "not configured" rather than as enforcement.
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "mode:")
		if !ok {
			continue
		}
		if salvagedMode(strings.Trim(strings.TrimSpace(rest), "\"'")).Mode == EgressEnforce {
			return Egress{Mode: EgressEnforce}
		}
	}
	return Egress{}
}

// discardedConfig is the resolved answer for a whole-file discard: the contained
// default, carrying the salvaged egress POSTURE and nothing else from the file.
// The Allow field is dropped explicitly rather than by omission, so a future
// caller cannot pass an allowlist through here by widening the argument.
func discardedConfig(egress Egress) Config {
	cfg := DefaultConfig()
	cfg.Egress = Egress{Mode: egress.Mode}
	return cfg
}

// egressEffect renders the egress half of a whole-file discard's Effect. A
// discard now has two possible egress outcomes, so the operator must be told
// which one they got: "running contained" is no longer the whole story.
func egressEffect(egress Egress) string {
	if egress.Mode == EgressEnforce {
		return "; the file named an enforcing egress posture, so egress is ENFORCED with an EMPTY allowlist — no declared destination is salvaged from a config that could not be trusted, so every destination is denied"
	}
	return "; no enforcing egress posture was recoverable from the file, so egress is unrestricted (the allow-all default)"
}

// resolve turns a parsed file into a consistent Config plus diagnostics.
func (raw rawConfig) resolve(path string) (Config, []Warning) {
	cfg := DefaultConfig()
	var warns []Warning

	// --- containment ------------------------------------------------------
	//
	// handler: is authoritative when present; contained: is the shorthand.
	// When both are present and disagree, the explicit handler wins and the
	// contradiction is reported. That direction is deliberate: `contained:
	// true` is the line every example file already carries, so an operator who
	// edits only `handler:` must not be silently ignored — being ignored is
	// what sends people looking for an env-var opt-out that must never exist.
	switch {
	case raw.Handler != nil:
		handler, ok := parseHandler(*raw.Handler)
		if !ok {
			// We cannot tell what substrate was meant. Discard the file
			// wholesale rather than partially honouring a config we do not
			// understand: its other fields (image, env_passthrough) widen what
			// a command can see, and they were written by the same hand that
			// typed a substrate that does not exist.
			//
			// The egress POSTURE is the sole exception, and it is not an
			// exception to that reasoning but an application of it: discarding
			// it would WIDEN what the command can reach, because the egress
			// default is the permissive side. The declared allowlist is
			// discarded with everything else. See salvageEgressPosture.
			egress := Egress{}
			if raw.Egress != nil && raw.Egress.Mode != nil {
				egress = salvagedMode(*raw.Egress.Mode)
			}
			return discardedConfig(egress), append(warns, Warning{
				Reason: WarnUnknownHandler,
				Path:   path,
				Detail: fmt.Sprintf("handler: %q is not %q or %q", *raw.Handler, HandlerContainer, HandlerHost),
				Effect: "unknown substrate named; the whole config was discarded and the sandbox is running contained on the container substrate" + egressEffect(egress),
			})
		}
		cfg.Handler = handler
		cfg.Contained = handler != HandlerHost

		if raw.Contained != nil && *raw.Contained != cfg.Contained {
			warns = append(warns, Warning{
				Reason: WarnContradictory,
				Path:   path,
				Detail: fmt.Sprintf("contained: %v disagrees with handler: %q", *raw.Contained, handler),
				Effect: fmt.Sprintf("the explicit handler wins; the sandbox is running %s on the %q substrate",
					containedWord(cfg.Contained), handler),
			})
		}

	case raw.Contained != nil:
		cfg.Contained = *raw.Contained
		if cfg.Contained {
			cfg.Handler = HandlerContainer
		} else {
			cfg.Handler = HandlerHost
		}
	}

	// --- benign fields ----------------------------------------------------

	if raw.Image != nil {
		cfg.Image = strings.TrimSpace(*raw.Image)
	}

	cfg.EnvPassthrough = cleanPassthrough(raw.EnvPassthrough)

	if raw.Pool != nil && raw.Pool.IdleTTL != nil {
		ttl, err := time.ParseDuration(strings.TrimSpace(*raw.Pool.IdleTTL))
		switch {
		case err != nil:
			warns = append(warns, Warning{
				Reason: WarnBadIdleTTL,
				Path:   path,
				Detail: fmt.Sprintf("pool.idle_ttl: %q is not a duration (want e.g. 5m)", *raw.Pool.IdleTTL),
				Effect: "using the default idle TTL of " + DefaultIdleTTL.String(),
			})
		case ttl <= 0:
			// Zero is the dangerous one: it would read as "reap on release",
			// tearing down every warm Runner the instant it is returned. A bad
			// value must fall back to the default, never to the zero value.
			warns = append(warns, Warning{
				Reason: WarnBadIdleTTL,
				Path:   path,
				Detail: fmt.Sprintf("pool.idle_ttl: %q is not positive", *raw.Pool.IdleTTL),
				Effect: "using the default idle TTL of " + DefaultIdleTTL.String(),
			})
		default:
			cfg.IdleTTL = ttl
		}
	}

	// --- limits & concurrency --------------------------------------------
	//
	// Both are POSTURE-FREE here: the loader parses only what the operator wrote
	// and leaves every unspecified field unset (nil). NewService fills the unset
	// fields with posture-appropriate defaults. A bad value degrades to unset
	// (⇒ a later default) with a loud warning, NEVER to a zero cap or a zero
	// bound — a zero cap some runtimes read as "unlimited", and a zero backstop
	// would refuse every Exec.

	if raw.Limits != nil {
		warns = raw.Limits.resolve(path, &cfg.Limits, warns)
	}
	if raw.Concurrency != nil {
		warns = raw.Concurrency.resolve(path, &cfg.Concurrency, warns)
	}

	// --- egress -----------------------------------------------------------
	//
	// Also posture-free, but for a different reason: egress posture is chosen by
	// the operator's explicit egress.mode knob and by nothing else. Unlike the
	// blocks above, a value here cannot degrade to "unset ⇒ a later default",
	// because the default IS the permissive posture. So a value the loader
	// cannot trust degrades toward ENFORCEMENT WITH AN EMPTY ALLOWLIST instead.
	if raw.Egress != nil {
		warns = raw.Egress.resolve(path, &cfg.Egress, warns)
	}

	return cfg, warns
}

// resolve applies the egress: block. A posture the loader cannot read degrades to
// enforcement, never to the permissive default. An allow list is ignored loudly:
// there is no proxy to honour one, so enforce is always deny-all.
func (raw rawEgress) resolve(path string, out *Egress, warns []Warning) []Warning {
	if raw.Mode != nil {
		mode, ok := parseEgressMode(*raw.Mode)
		if !ok {
			*out = Egress{Mode: EgressEnforce}
			return append(warns, Warning{
				Reason: WarnUnknownEgressMode,
				Path:   path,
				Detail: fmt.Sprintf("egress.mode: %q is not %q or %q", *raw.Mode, EgressAllowAll, EgressEnforce),
				Effect: "unknown egress mode named; egress is ENFORCED, so every destination is denied",
			})
		}
		out.Mode = mode
	}
	if len(raw.Allow) > 0 {
		warns = append(warns, Warning{
			Reason: WarnBadEgress,
			Path:   path,
			Detail: "egress.allow is not supported",
			Effect: "the allow list was ignored; mode enforce is deny-all and mode allow-all is unrestricted",
		})
	}
	return warns
}

// parseEgressMode maps a configured posture name onto the closed enum.
//
// Case-insensitive and space-tolerant but otherwise exact: no aliasing and, above
// all, NO FALLBACK. An unrecognised name is reported as unrecognised, because the
// alternative — quietly picking one — would pick the permissive one.
func parseEgressMode(v string) (EgressMode, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case EgressAllowAll.String():
		return EgressAllowAll, true
	case EgressEnforce.String():
		return EgressEnforce, true
	default:
		return EgressEnforce, false
	}
}

// resolve fills out from a parsed limits: block, appending a Warning for
// every field it could not honour. It receives out by pointer and returns
// warns so the caller's diagnostics accumulate.
func (raw rawLimits) resolve(path string, out *Limits, warns []Warning) []Warning {
	bad := func(field, detail string) {
		warns = append(warns, Warning{
			Reason: WarnBadLimit,
			Path:   path,
			Detail: fmt.Sprintf("limits.%s: %s", field, detail),
			Effect: "ignoring this cap; the posture default applies",
		})
	}

	if raw.Memory != nil {
		if b, ok := parseBytes(*raw.Memory); ok {
			out.MemoryBytes = &b
		} else {
			bad("memory", fmt.Sprintf("%q is not a positive byte size (want e.g. 2g)", *raw.Memory))
		}
	}
	if raw.CPUs != nil {
		if s, ok := parseCPUs(*raw.CPUs); ok {
			out.CPUs = &s
		} else {
			bad("cpus", fmt.Sprintf("%q is not a positive decimal (want e.g. 2.0)", *raw.CPUs))
		}
	}
	if raw.Pids != nil {
		if *raw.Pids > 0 {
			v := *raw.Pids
			out.Pids = &v
		} else {
			bad("pids", fmt.Sprintf("%d is not positive", *raw.Pids))
		}
	}
	if raw.NoFile != nil {
		if *raw.NoFile > 0 {
			v := *raw.NoFile
			out.NoFile = &v
		} else {
			bad("nofile", fmt.Sprintf("%d is not positive", *raw.NoFile))
		}
	}
	if raw.Fsize != nil {
		if b, ok := parseBytes(*raw.Fsize); ok {
			out.FsizeBytes = &b
		} else {
			bad("fsize", fmt.Sprintf("%q is not a positive byte size (want e.g. 2g)", *raw.Fsize))
		}
	}
	if raw.PullTimeout != nil {
		if d, ok := parsePositiveDuration(*raw.PullTimeout); ok {
			out.PullTimeout = &d
		} else {
			bad("pull_timeout", fmt.Sprintf("%q is not a positive duration (want e.g. 2m)", *raw.PullTimeout))
		}
	}
	return warns
}

// resolve fills out from a parsed concurrency: block, appending a
// WarnBadConcurrency for every field it could not honour.
func (raw rawConcurrency) resolve(path string, out *Concurrency, warns []Warning) []Warning {
	bad := func(field, detail string) {
		warns = append(warns, Warning{
			Reason: WarnBadConcurrency,
			Path:   path,
			Detail: fmt.Sprintf("concurrency.%s: %s", field, detail),
			Effect: "ignoring this bound; the built-in default applies",
		})
	}
	count := func(field string, v *int64, dst **int64) {
		if v == nil {
			return
		}
		if *v > 0 {
			n := *v
			*dst = &n
		} else {
			bad(field, fmt.Sprintf("%d is not positive", *v))
		}
	}

	count("max_inflight", raw.MaxInflight, &out.MaxInflight)
	count("max_inflight_per_tenant", raw.MaxInflightPerTenant, &out.MaxInflightPerTenant)
	count("max_queued", raw.MaxQueued, &out.MaxQueued)
	if raw.NoteThreshold != nil {
		if d, ok := parsePositiveDuration(*raw.NoteThreshold); ok {
			out.NoteThreshold = &d
		} else {
			bad("note_threshold", fmt.Sprintf("%q is not a positive duration (want e.g. 2s)", *raw.NoteThreshold))
		}
	}
	return warns
}

// parseHandler maps a configured substrate name onto the closed enum.
//
// Matching is case-insensitive and space-tolerant but otherwise exact: no
// prefix matching, no aliasing, and above all no fallback. An unrecognised name
// is reported as unrecognised, because the alternative — quietly picking one —
// is how a typo becomes an uncontained shell.
func parseHandler(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case HandlerContainer:
		return HandlerContainer, true
	case HandlerHost:
		return HandlerHost, true
	default:
		return "", false
	}
}

// cleanPassthrough copies the operator's declared variable names, dropping
// blanks and preserving order. It returns nil for an empty result so a Config
// with no passthrough compares equal to the default.
func cleanPassthrough(in []string) []string {
	var out []string
	for _, k := range in {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func containedWord(contained bool) string {
	if contained {
		return "contained"
	}
	return "UNCONTAINED"
}

// parseBytes parses a human byte size ("2g", "512m", "1048576") into a positive
// count of bytes. It accepts an optional k/m/g/t suffix (binary, 1024-based, to
// match how docker interprets --memory suffixes) and a bare integer. It returns
// (0, false) for anything unparsable or non-positive, so a bad value degrades to
// a posture default rather than to a zero cap.
func parseBytes(v string) (int64, bool) {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return 0, false
	}
	var mult int64 = 1
	switch s[len(s)-1] {
	case 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g':
		mult, s = 1<<30, s[:len(s)-1]
	case 't':
		mult, s = 1<<40, s[:len(s)-1]
	}
	s = strings.TrimSpace(s)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	// Guard the multiply against overflow: a wildly large suffixed value is a
	// typo, not a cap, and a wrapped negative would read as a bad flag.
	if n > (1<<62)/mult {
		return 0, false
	}
	return n * mult, true
}

// parseCPUs validates a --cpus decimal and returns it in a canonical rendered
// form. It parses to a float only to REJECT nonsense and enforce positivity;
// the returned string is re-rendered deterministically (strconv, not %v) so argv
// is locale-independent and golden-testable. A trailing ".0" is preserved for a
// whole number so the flag reads as the decimal docker expects.
func parseCPUs(v string) (string, bool) {
	s := strings.TrimSpace(v)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", false
	}
	return renderCPUs(f), true
}

// renderCPUs renders a positive CPU fraction deterministically: the shortest
// decimal that round-trips, but always with at least one fractional digit so a
// whole number reads as "2.0" rather than "2".
func renderCPUs(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// parsePositiveDuration parses a Go duration string and requires it be positive.
// It returns (0, false) for an unparsable or non-positive value, matching the
// idle_ttl discipline: a bad duration degrades to a default, never to zero.
func parsePositiveDuration(v string) (time.Duration, bool) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
