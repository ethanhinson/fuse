package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Error sentinels for remote execution.
var (
	ErrMaxDepthExceeded     = errors.New("agent: max spawn depth exceeded")
	ErrUnknownModel         = errors.New("agent: unknown model")
	ErrNoRemoteExecutor     = errors.New("agent: no remote executor registered")
	ErrRemoteDispatchFailed = errors.New("agent: remote dispatch failed")
	ErrRemoteStreamLost     = errors.New("agent: remote SSE stream lost")
	ErrIntentResolveFailed  = errors.New("agent: intent plugin resolve failed")
	ErrSecretNotFound       = errors.New("agent: secret not found")
	ErrSecretsStoreFailure  = errors.New("agent: secrets store failure")
	ErrEncryptionFailure    = errors.New("agent: encryption failure")
)

// RemoteDispatchRequest is sent to a remote executor at dispatch time.
type RemoteDispatchRequest struct {
	Label        string `json:"label"`
	Task         string `json:"task"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	ModelID      string `json:"model_id,omitempty"`
	MaxTurns     int    `json:"max_turns,omitempty"`
	MaxTokens    int    `json:"max_tokens,omitempty"`
	ParentNodeID string `json:"parent_node_id,omitempty"`
	Depth        int    `json:"depth"`

	Image string            `json:"image,omitempty"`
	Env   map[string]string `json:"env,omitempty"`

	Clone  *GitCloneSpec `json:"clone,omitempty"`
	Bundle []byte        `json:"bundle,omitempty"`

	WriteBackBranch string `json:"write_back_branch,omitempty"`
	WriteBackRemote string `json:"write_back_remote,omitempty"`
	WriteBackToken  string `json:"write_back_token,omitempty"`

	Files map[string][]byte `json:"files,omitempty"`

	Secrets          map[string]string `json:"secrets,omitempty"`
	EncryptedSecrets []byte            `json:"encrypted_secrets,omitempty"`
	SecretsAlgorithm string            `json:"secrets_algorithm,omitempty"`
}

// RemoteExecutor dispatches a job to a remote agent runtime.
type RemoteExecutor interface {
	Dispatch(ctx context.Context, req RemoteDispatchRequest) (<-chan AgentEvent, error)
}

// SSERemoteExecutor dispatches to a remote runner via HTTP + Server-Sent Events.
type SSERemoteExecutor struct {
	BaseURL       string
	TokenSecret   string // secret name; resolved via SecretsStore at dispatch time
	HTTPClient    *http.Client
	MaxRetries    int
	PublicKey     string // optional: remote's age public key; enables encrypted bundle mode
	resolvedToken string // set at spawn time from the SecretsStore
}

type dispatchResponse struct {
	JobID     string `json:"job_id"`
	StreamURL string `json:"stream_url"`
}

// Dispatch sends a RemoteDispatchRequest to the remote and returns an event channel.
func (e *SSERemoteExecutor) Dispatch(ctx context.Context, req RemoteDispatchRequest) (<-chan AgentEvent, error) {
	client := e.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	maxRetries := e.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal request: %v", ErrRemoteDispatchFailed, err)
	}

	dispURL := strings.TrimRight(e.BaseURL, "/") + "/v1/agents/dispatch"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", dispURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrRemoteDispatchFailed, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if e.resolvedToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.resolvedToken)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRemoteDispatchFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w: status %d: %s", ErrRemoteDispatchFailed, resp.StatusCode, string(b))
	}

	var dr dispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrRemoteDispatchFailed, err)
	}

	ch := make(chan AgentEvent, 32)
	baseURL := strings.TrimRight(e.BaseURL, "/")
	go func() {
		defer close(ch)
		var lastSeq int64
		for attempt := 0; attempt <= maxRetries; attempt++ {
			streamURL := dr.StreamURL
			if streamURL == "" {
				streamURL = baseURL + "/v1/agents/" + dr.JobID + "/stream"
			}

			sseReq, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
			if err != nil {
				break
			}
			sseReq.Header.Set("Accept", "text/event-stream")
			if lastSeq > 0 {
				sseReq.Header.Set("Last-Event-ID", fmt.Sprintf("%d", lastSeq))
			}
			if e.resolvedToken != "" {
				sseReq.Header.Set("Authorization", "Bearer "+e.resolvedToken)
			}

			sseResp, err := client.Do(sseReq)
			if err != nil {
				if ctx.Err() != nil {
					e.sendCancel(client, baseURL, dr.JobID)
					return
				}
				backoff := time.Duration(200<<uint(attempt)) * time.Millisecond
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				continue
			}

			streamDone := e.readSSEStream(ctx, sseResp.Body, ch, &lastSeq)
			sseResp.Body.Close()
			if streamDone {
				return
			}
			if ctx.Err() != nil {
				e.sendCancel(client, baseURL, dr.JobID)
				return
			}
			// Stream dropped; back-off before retry.
			backoff := time.Duration(200<<uint(attempt)) * time.Millisecond
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				e.sendCancel(client, baseURL, dr.JobID)
				return
			}
		}
		// Exhausted retries.
		ch <- AgentEvent{
			Kind:    KindError,
			Name:    "stream_lost",
			Payload: map[string]any{"error": ErrRemoteStreamLost.Error()},
			TS:      time.Now(),
		}
	}()

	return ch, nil
}

// readSSEStream reads SSE events from body, sends them on ch, and returns true
// when a terminal event (done/error) is received.
func (e *SSERemoteExecutor) readSSEStream(ctx context.Context, body io.Reader, ch chan<- AgentEvent, lastSeq *int64) bool {
	scanner := bufio.NewScanner(body)
	var evtID, evtData string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if evtData == "" {
				continue
			}
			var evt AgentEvent
			if err := json.Unmarshal([]byte(evtData), &evt); err == nil {
				if evtID != "" {
					fmt.Sscanf(evtID, "%d", lastSeq)
					evt.Seq = *lastSeq
				}
				select {
				case ch <- evt:
				case <-ctx.Done():
					return false
				}
				if evt.Kind == KindDone || evt.Kind == KindError {
					return true
				}
			}
			evtID, evtData = "", ""
			continue
		}
		if after, ok := strings.CutPrefix(line, "id:"); ok {
			evtID = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "data:"); ok {
			evtData = strings.TrimSpace(after)
		}
	}
	return false
}

// sendCancel sends a best-effort DELETE to cancel a remote job.
func (e *SSERemoteExecutor) sendCancel(client *http.Client, baseURL, jobID string) {
	cancelURL := baseURL + "/v1/agents/" + jobID
	req, err := http.NewRequest("DELETE", cancelURL, nil)
	if err != nil {
		return
	}
	if e.resolvedToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.resolvedToken)
	}
	client.Do(req) //nolint:errcheck
}
