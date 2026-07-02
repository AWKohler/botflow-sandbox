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

type previewManager struct {
	mu                     sync.Mutex
	bindHost, externalHost string
	start, end             int
	listeners              map[int]net.Listener
	logger                 *slog.Logger
}

func newPreviewManager(c apiConfig, logger *slog.Logger) *previewManager {
	return &previewManager{bindHost: c.PreviewBindHost, externalHost: c.PreviewExternalHost, start: c.PreviewPortStart, end: c.PreviewPortEnd, listeners: make(map[int]net.Listener), logger: logger}
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
	return apimodel.Route{URL: "http://" + net.JoinHostPort(p.externalHost, strconv.Itoa(hostPort)), Subdomain: "local-" + sessionID + "-" + strconv.Itoa(guestPort), Port: guestPort, HostPort: hostPort}, nil
}
func (p *previewManager) close(routes []apimodel.Route) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, route := range routes {
		if ln := p.listeners[route.HostPort]; ln != nil {
			_ = ln.Close()
			delete(p.listeners, route.HostPort)
		}
	}
}
