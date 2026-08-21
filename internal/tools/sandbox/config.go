package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

// Config is the resolved sandbox configuration.
//
// A Config obtained from LoadConfig or DefaultConfig is always internally
// consistent and always safe to act on directly: there is no "unset" state and
// no error state a caller must remember to check before using it. Every path
// that could not produce a trustworthy answer produces the contained default
// instead.
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
	Contained      *bool    `yaml:"contained"`
	Handler        *string  `yaml:"handler"`
	Image          *string  `yaml:"image"`
	EnvPassthrough []string `yaml:"env_passthrough"`
	Pool           *rawPool `yaml:"pool"`
}

type rawPool struct {
	// IdleTTL is decoded as a string rather than a time.Duration so that a
	// value YAML happens to read as a number ("300") degrades to a warning
	// rather than to a silently different meaning.
	IdleTTL *string `yaml:"idle_ttl"`
}

// LoadConfig reads the operator off-switch file at <root>/.fuse/sandbox.local.yml.
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
		return DefaultConfig(), []Warning{{
			Reason: WarnMalformed,
			Path:   path,
			Detail: strings.TrimSpace(err.Error()),
			Effect: "config file could not be parsed; running contained on the container substrate",
		}}
	}

	return raw.resolve(path)
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
			return DefaultConfig(), append(warns, Warning{
				Reason: WarnUnknownHandler,
				Path:   path,
				Detail: fmt.Sprintf("handler: %q is not %q or %q", *raw.Handler, HandlerContainer, HandlerHost),
				Effect: "unknown substrate named; the whole config was discarded and the sandbox is running contained on the container substrate",
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

	return cfg, warns
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
