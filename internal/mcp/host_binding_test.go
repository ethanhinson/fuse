package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// authSink records every Authorization header a server sees.
type authSink struct {
	mu    sync.Mutex
	auths []string
}

func (a *authSink) add(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.auths = append(a.auths, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
}

func (a *authSink) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

// routeHosts makes http.DefaultTransport dial the given listener for each fake
// host (a "host:port" key), so tests can use distinct hostnames, including a
// subdomain, without DNS.
func routeHosts(t *testing.T, routes map[string]string) {
	t.Helper()
	prev := http.DefaultTransport
	tr := http.DefaultTransport.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: 5 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if to, ok := routes[addr]; ok {
			addr = to
		}
		return d.DialContext(ctx, network, addr)
	}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = prev })
}

func listenerAddr(srv *httptest.Server) string { return srv.Listener.Addr().String() }

func TestHTTPClientRefusesEndpointOnAnotherHost(t *testing.T) {
	other := &authSink{}
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.add(r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", "http://other.test:"+portOf(second)+"/messages")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer first.Close()
	routeHosts(t, map[string]string{
		"main.test:" + portOf(first):   listenerAddr(first),
		"other.test:" + portOf(second): listenerAddr(second),
	})

	_, err := newHTTPClient("srv", "http://main.test:"+portOf(first), "tok-secret")
	if err == nil || !strings.Contains(err.Error(), "endpoint event names another host") {
		t.Fatalf("err = %v, want endpoint another host refusal", err)
	}
	if strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("error leaks the token: %v", err)
	}
	if got := other.seen(); len(got) != 0 {
		t.Fatalf("second server saw requests: %v", got)
	}
}

func TestHTTPClientRefusesRedirects(t *testing.T) {
	for _, target := range []string{"http://other.test", "http://sub.main.test"} {
		t.Run(target, func(t *testing.T) {
			other := &authSink{}
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				other.add(r)
				w.WriteHeader(http.StatusOK)
			}))
			defer second.Close()
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target+":"+portOf(second)+r.URL.Path, http.StatusFound)
			}))
			defer first.Close()
			routeHosts(t, map[string]string{
				"main.test:" + portOf(first):      listenerAddr(first),
				"other.test:" + portOf(second):    listenerAddr(second),
				"sub.main.test:" + portOf(second): listenerAddr(second),
			})
			base := "http://main.test:" + portOf(first)

			// SSE dial: the 302 surfaces as an ordinary non-200 failure.
			_, err := newHTTPClient("srv", base, "tok-secret")
			if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
				t.Fatalf("sse err = %v, want HTTP 302 failure", err)
			}

			// Streamable: the POST gets the same treatment.
			sc, err := newStreamableHTTPClient("srv", base, "tok-secret", config.MCPAuthConfig{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = sc.call(context.Background(), "tools/list", nil)
			if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
				t.Fatalf("streamable err = %v, want HTTP 302 failure", err)
			}
			if got := other.seen(); len(got) != 0 {
				t.Fatalf("redirect target saw requests: %v", got)
			}
		})
	}
}

func portOf(srv *httptest.Server) string {
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return p
}
