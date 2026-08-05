package agent

import "context"

// IntentPlugin resolves the execution context for a remote subagent and handles
// write-back after it completes.
type IntentPlugin interface {
	Name() string
	// Resolve is called synchronously before dispatch. Errors fail the spawn.
	Resolve(ctx context.Context, opts SpawnOpts) (*RemoteContext, error)
	// Collect is called asynchronously after the remote agent completes.
	// Errors are non-fatal.
	Collect(ctx context.Context, done SpawnDone, rc *RemoteContext) error
}

// RemoteContext is the resolved execution context for a remote spawn.
type RemoteContext struct {
	Image                string
	Env                  map[string]string
	Clone                *GitCloneSpec
	Bundle               []byte
	WriteBackBranch      string
	WriteBackRemote      string
	WriteBackTokenSecret string            // secret name; resolved in spawn path
	Files                map[string][]byte // container path → content
	PluginMeta           map[string]string // opaque; passed to Collect; not sent to remote
	SecretRefs           []string          // additional secret names for the container
}

// GitCloneSpec describes how to clone a git repo into a remote container.
type GitCloneSpec struct {
	URL           string
	Ref           string
	TokenSecret   string // secret name; resolved in the spawn path
	ResolvedToken string // resolved value; only its value travels in RemoteDispatchRequest
}

// NilIntentPlugin is a no-op intent plugin used when no plugin is configured.
type NilIntentPlugin struct{}

func (NilIntentPlugin) Name() string { return "nil" }
func (NilIntentPlugin) Resolve(_ context.Context, _ SpawnOpts) (*RemoteContext, error) {
	return &RemoteContext{}, nil
}
func (NilIntentPlugin) Collect(_ context.Context, _ SpawnDone, _ *RemoteContext) error {
	return nil
}
