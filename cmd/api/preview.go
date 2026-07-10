package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"

	"github.com/ai-club/sandbox-host/internal/apimodel"
)

// previewTarget is the subdomain router's view of one open preview route:
// where to proxy, and a ready-built proxy so the hot path allocates nothing.
type previewTarget struct {
	sessionID string
	guestIP   string
	guestPort int
	proxy     *httputil.ReverseProxy
}

type previewManager struct {
	mu                     sync.Mutex
	bindHost, externalHost string
	domain                 string
	start, end             int
	listeners              map[int]net.Listener
	// routes indexes open preview routes by subdomain label for the
	// tunnel-fronted router. Only populated when domain != "". Rebuilt on
	// restart because reconcileActiveSessions re-opens every live route
	// through open().
	routes map[string]previewTarget
	logger *slog.Logger
}

func newPreviewManager(c apiConfig, logger *slog.Logger) *previewManager {
	return &previewManager{bindHost: c.PreviewBindHost, externalHost: c.PreviewExternalHost, domain: c.PreviewDomain, start: c.PreviewPortStart, end: c.PreviewPortEnd, listeners: make(map[int]net.Listener), routes: make(map[string]previewTarget), logger: logger}
}
func (p *previewManager) open(ctx context.Context, sessionID, guestIP string, guestPort int) (apimodel.Route, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ln net.Listener
	var hostPort int
	var err error
	for port := p.start; port <= p.end; port++ {
		if _, used := p.listeners[port]; used {
			continue
		}
		ln, err = net.Listen("tcp", net.JoinHostPort(p.bindHost, strconv.Itoa(port)))
		if err == nil {
			hostPort = port
			break
		}
	}
	if ln == nil {
		return apimodel.Route{}, fmt.Errorf("no preview port available: %w", err)
	}
	p.listeners[hostPort] = ln
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(guestIP, strconv.Itoa(guestPort))}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "sandbox preview unavailable", http.StatusBadGateway)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		proxy.ServeHTTP(w, r)
	})}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.logger.Warn("preview listener failed", "session", sessionID, "hostPort", hostPort, "error", err)
		}
	}()
	go func() { <-ctx.Done(); _ = server.Close() }()

	subdomain := "local-" + sessionID + "-" + strconv.Itoa(guestPort)
	routeURL := "http://" + net.JoinHostPort(p.externalHost, strconv.Itoa(hostPort))
	if p.domain != "" {
		// Tunnel-routed: mint a fresh high-entropy label per route (96 bits;
		// the legacy "local-<sessionID>-<port>" form exceeds the 63-char DNS
		// label limit) and advertise the public HTTPS URL. The tailnet port
		// listener above stays open as an operator back-door.
		subdomain = randomID("p", 12) + "-" + strconv.Itoa(guestPort)
		routeURL = "https://" + subdomain + "." + p.domain
		p.routes[subdomain] = previewTarget{sessionID: sessionID, guestIP: guestIP, guestPort: guestPort, proxy: proxy}
	}
	return apimodel.Route{URL: routeURL, Subdomain: subdomain, Port: guestPort, HostPort: hostPort}, nil
}
func (p *previewManager) close(routes []apimodel.Route) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, route := range routes {
		if ln := p.listeners[route.HostPort]; ln != nil {
			_ = ln.Close()
			delete(p.listeners, route.HostPort)
		}
		delete(p.routes, route.Subdomain)
	}
}

// lookup resolves a subdomain label to its open preview target. Used by the
// tunnel-fronted preview router on every request.
func (p *previewManager) lookup(subdomain string) (previewTarget, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	target, ok := p.routes[subdomain]
	return target, ok
}
