package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// agentProxy is deliberately only an authenticated streaming transport. The
// Python service owns workspaces, sessions, model execution, interruption and
// billing; duplicating any of that here would create two sources of truth.
type agentProxy struct {
	base    string
	maxRuns int
	client  *http.Client

	mu   sync.Mutex
	runs map[string]int
}

func newAgentProxy(base string, maxRuns int) *agentProxy {
	if maxRuns < 1 {
		maxRuns = 2
	}
	return &agentProxy{
		base:    strings.TrimRight(base, "/"),
		maxRuns: maxRuns,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
		runs: make(map[string]int),
	}
}

func (p *agentProxy) acquire(userID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runs[userID] >= p.maxRuns {
		return false
	}
	p.runs[userID]++
	return true
}

func (p *agentProxy) release(userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runs[userID] <= 1 {
		delete(p.runs, userID)
		return
	}
	p.runs[userID]--
}

func (s *Server) handleAgentProxy(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		message := "sign in at " + s.cfg.LoginURL + " to use the agent"
		if bearerToken(ctx) != "" {
			message = "invalid or revoked API key"
		}
		sendError(ctx, http.StatusUnauthorized, message)
		return
	}
	path := string(ctx.Path())
	controlsRun := string(ctx.Method()) == http.MethodPost && path == "/v1/agent/runs"
	if controlsRun && user.Credits() < 1 {
		s.bill.grantWelcome(user)
		user = s.auth.userByID(user.ID)
	}
	if controlsRun && (user == nil || user.Credits() < 1) {
		sendError(ctx, http.StatusPaymentRequired, "out of credits — top up at "+s.cfg.AccountURL)
		return
	}
	if strings.TrimSpace(s.cfg.AgentToken) == "" {
		sendError(ctx, http.StatusServiceUnavailable, "agent service is not configured")
		return
	}

	proxy := s.agent
	if proxy == nil {
		proxy = newAgentProxy(s.cfg.AgentURL, s.cfg.MaxAgentRuns)
		s.agent = proxy
	}

	if controlsRun && !proxy.acquire(user.ID) {
		sendError(ctx, http.StatusTooManyRequests,
			fmt.Sprintf("at most %d agent runs may be active for this account", proxy.maxRuns))
		return
	}
	var released sync.Once
	release := func() {
		if controlsRun {
			released.Do(func() { proxy.release(user.ID) })
		}
	}

	upstreamCtx, cancel := context.WithCancel(context.Background())
	target := proxy.base + path
	if query := string(ctx.URI().QueryString()); query != "" {
		target += "?" + query
	}
	req, err := http.NewRequestWithContext(upstreamCtx, string(ctx.Method()), target,
		bytes.NewReader(ctx.PostBody()))
	if err != nil {
		cancel()
		release()
		sendError(ctx, http.StatusBadGateway, "agent proxy request failed")
		return
	}
	if contentType := string(ctx.Request.Header.ContentType()); contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "text/event-stream, application/json")
	// The shared backend gets a service credential and the already-resolved
	// mojojojo subject. Browser sessions and developer API keys stop here.
	req.Header.Set("Authorization", "Bearer "+s.cfg.AgentToken)
	req.Header.Set("X-Mojojojo-User", user.ID)

	resp, err := proxy.client.Do(req)
	if err != nil {
		cancel()
		release()
		log.Printf("agent proxy: %v", err)
		sendError(ctx, http.StatusBadGateway, "agent service is unavailable")
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		defer cancel()
		defer release()
		defer resp.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if readErr != nil {
			sendError(ctx, http.StatusBadGateway, "agent service returned an unreadable response")
			return
		}
		ctx.SetStatusCode(resp.StatusCode)
		if contentType != "" {
			ctx.Response.Header.SetContentType(contentType)
		}
		ctx.Response.SetBody(body)
		return
	}

	ctx.SetStatusCode(resp.StatusCode)
	ctx.Response.Header.SetContentType(contentType)
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("X-Accel-Buffering", "no")
	if runID := resp.Header.Get("X-Run-ID"); runID != "" {
		ctx.Response.Header.Set("X-Run-ID", runID)
	}
	ctx.SetBodyStreamWriter(func(writer *bufio.Writer) {
		defer cancel()
		defer release()
		defer resp.Body.Close()
		streamAgentSSE(writer, resp.Body, cancel)
	})
}

// streamAgentSSE retains the upstream bytes exactly. Flushing at every blank
// line preserves SSE event framing and makes reasoning/tool events visible as
// soon as the agent emits them.
func streamAgentSSE(writer *bufio.Writer, upstream io.Reader, cancel context.CancelFunc) {
	reader := bufio.NewReader(upstream)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if _, writeErr := writer.WriteString(line); writeErr != nil {
				cancel()
				return
			}
			if line == "\n" || line == "\r\n" {
				if flushErr := writer.Flush(); flushErr != nil {
					cancel()
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("agent stream: %v", err)
			}
			_ = writer.Flush()
			return
		}
	}
}
