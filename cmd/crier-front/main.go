// Command crier-front is the shipped authenticating front for an auth-enabled
// crier relay (DF-CRIER-333).
//
// When a relay runs with CR_AUTH_TOKEN set, every non-exempt path demands
// `Authorization: Bearer <token>` — including ordinary navigation, so a browser
// opening GET /chat gets 401 because a browser cannot attach that header. The
// supported arrangement is a front process that holds the credential and injects
// it on the wire, so the token never reaches page source, cookies, or
// localStorage. This binary is that process.
//
// Design constraints it exists to satisfy:
//   - the shared token is read from the environment and never logged, echoed in
//     a header, or written to disk;
//   - a client-supplied Authorization header is always replaced, never forwarded,
//     so a caller cannot present a different credential through the front;
//   - only the path space of the upstream relay is exposed — the front has no
//     filesystem access and no static file serving of its own;
//   - it binds loopback by default.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defaultListen   = "127.0.0.1:8770"
	defaultUpstream = "http://127.0.0.1:8765"
	// healthPath is served by the front itself so an orchestrator can check the
	// front without needing the credential.
	healthPath = "/front-health"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("crier-front: %v", err)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("crier-front", flag.ContinueOnError)
	listen := fs.String("listen", envOr("CR_FRONT_LISTEN", defaultListen), "listen address (host:port); loopback by default")
	upstream := fs.String("upstream", envOr("CR_FRONT_UPSTREAM", defaultUpstream), "upstream crier relay base URL")
	tokenEnv := fs.String("token-env", "CR_AUTH_TOKEN", "environment variable holding the shared bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := strings.TrimSpace(os.Getenv(*tokenEnv))
	if token == "" {
		return fmt.Errorf("%s is empty: an auth-enabled relay needs the shared bearer token, and the front will not start without it", *tokenEnv)
	}
	target, err := url.Parse(strings.TrimSpace(*upstream))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return fmt.Errorf("invalid upstream %q: want an absolute base URL such as %s", *upstream, defaultUpstream)
	}

	proxy, err := newProxy(target, token)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"front":"ok","upstream":%q}`+"\n", target.String()); err != nil {
			log.Printf("crier-front: health write failed: %v", err)
		}
	})
	mux.Handle("/", proxy)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: the relay streams chat reads and SSE, and a write
		// deadline would sever a long-lived stream mid-body.
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *listen, err)
	}
	log.Printf("crier-front: listening on %s, injecting bearer for upstream %s", ln.Addr(), target)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-sigCh:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

// newProxy builds the reverse proxy that carries the credential. The token is
// captured here, from the environment the process was started with, and is never
// read from an incoming request.
//
// The proxy is built on Rewrite rather than Director so the inbound request can
// be inspected before the outbound one is formed; ReverseProxy accepts exactly
// one of the two, so NewSingleHostReverseProxy (which sets Director) is not used.
func newProxy(target *url.URL, token string) (*httputil.ReverseProxy, error) {
	if target == nil {
		return nil, errors.New("nil upstream")
	}
	proxy := &httputil.ReverseProxy{
		// Stream bodies straight through: chat reads and SSE must not be buffered.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			// Always replace, never forward, the client's credential.
			pr.Out.Header.Set("Authorization", "Bearer "+token)
			pr.SetXForwarded()
		},
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// The upstream error is reported without the request headers, which
		// carry the token.
		log.Printf("crier-front: upstream %s %s failed: %v", r.Method, r.URL.Path, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		if _, err := fmt.Fprint(w, `{"error":"FRONT_UPSTREAM_UNAVAILABLE"}`+"\n"); err != nil {
			log.Printf("crier-front: error write failed: %v", err)
		}
	}
	return proxy, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
