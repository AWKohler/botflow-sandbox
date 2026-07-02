package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/ai-club/sandbox-host/internal/guestproto"
	"github.com/ai-club/sandbox-host/internal/runtimeproto"
)

type runtimeClient struct{ socket string }

func (c runtimeClient) do(ctx context.Context, method, path string, body, out any) error {
	return unixRequest(ctx, c.socket, method, path, body, out)
}
func (c runtimeClient) sessions(ctx context.Context) (map[string]bool, error) {
	var out struct {
		Sessions []string `json:"sessions"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/sessions", nil, &out); err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(out.Sessions))
	for _, id := range out.Sessions {
		set[id] = true
	}
	return set, nil
}
func (c runtimeClient) create(ctx context.Context, body runtimeproto.CreateSessionRequest) (runtimeproto.Session, error) {
	var out runtimeproto.Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions", body, &out)
	return out, err
}
func (c runtimeClient) stop(ctx context.Context, id string, body runtimeproto.StopSessionRequest) (runtimeproto.StopSessionResponse, error) {
	var out runtimeproto.StopSessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+id+"/stop", body, &out)
	return out, err
}
func (c runtimeClient) updatePolicy(ctx context.Context, id string, body runtimeproto.UpdatePolicyRequest) error {
	return c.do(ctx, http.MethodPut, "/v1/sessions/"+id+"/network-policy", body, nil)
}
func (c runtimeClient) deleteSnapshot(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/snapshots/"+id, nil, nil)
}

func unixRequest(ctx context.Context, socket, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 45 * time.Second}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("runtime returned %d: %s", resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

type guestClient struct{ http *http.Client }

func newGuestClient() *guestClient {
	return &guestClient{http: &http.Client{Timeout: 0, Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConnsPerHost: 8, ResponseHeaderTimeout: 30 * time.Second}}}
}

func (g *guestClient) request(ctx context.Context, guestIP, token, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	return g.requestHeaders(ctx, guestIP, token, method, path, body, contentType, nil)
}

func (g *guestClient) requestHeaders(ctx context.Context, guestIP, token, method, path string, body io.Reader, contentType string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+net.JoinHostPort(guestIP, "1024")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return g.http.Do(req)
}
func (g *guestClient) json(ctx context.Context, guestIP, token, method, path string, in, out any) error {
	var reader io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	resp, err := g.request(ctx, guestIP, token, method, path, reader, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("guest returned %d: %s", resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}
func (g *guestClient) start(ctx context.Context, ip, token string, req guestproto.StartCommandRequest) (guestproto.Command, error) {
	var out guestproto.CommandResponse
	err := g.json(ctx, ip, token, http.MethodPost, "/v1/commands", req, &out)
	return out.Command, err
}
func (g *guestClient) get(ctx context.Context, ip, token, id string, wait bool) (guestproto.Command, error) {
	path := "/v1/commands/" + id
	if wait {
		path += "?wait=true"
	}
	var out guestproto.CommandResponse
	err := g.json(ctx, ip, token, http.MethodGet, path, nil, &out)
	return out.Command, err
}
