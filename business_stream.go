package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

type upstream struct {
	base, token string
	own         bool
}

var streamClient = &fasthttp.Client{MaxConnsPerHost: 256, ReadTimeout: 60 * time.Second, WriteTimeout: 30 * time.Second, StreamResponseBody: true}

func (s *Server) upstreamFor(b business) upstream {
	key := "MOJOJOJO_UPSTREAM_" + strings.ToUpper(strings.ReplaceAll(b.Slug, "-", "_"))
	def, tok := b.Upstream, s.cfg.CloudToken
	if b.Kind == "model" {
		// One modeld serves every model business: one URL and one token.
		def, tok = env("MOJOJOJO_MODELD_URL", def), env("MOJOJOJO_MODELD_TOKEN", tok)
	}
	if v := strings.TrimRight(env(key, def), "/"); v != "" {
		return upstream{base: v, token: env(key+"_TOKEN", tok), own: true}
	}
	return upstream{base: s.cfg.CloudURL, token: s.cfg.CloudToken}
}

func streamLimits() (time.Duration, int64) {
	secs, _ := strconv.Atoi(env("MOJOJOJO_STREAM_MAX_SECONDS", "300"))
	if secs <= 0 {
		secs = 300
	}
	mb, _ := strconv.Atoi(env("MOJOJOJO_STREAM_MAX_MB", "8"))
	if mb <= 0 {
		mb = 8
	}
	return time.Duration(secs) * time.Second, int64(mb) << 20
}

type doneTap struct {
	line      []byte
	event     string
	prompt    int
	completed int
	searches  int
	seen      bool
}

func (t *doneTap) feed(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(t.line)+len(p) <= 1<<16 {
				t.line = append(t.line, p...)
			}
			return
		}
		if len(t.line)+i <= 1<<16 {
			t.line = append(t.line, p[:i]...)
			t.consume(bytes.TrimRight(t.line, "\r"))
		}
		t.line = t.line[:0]
		p = p[i+1:]
	}
}

func (t *doneTap) consume(l []byte) {
	switch {
	case bytes.HasPrefix(l, []byte("event: ")):
		t.event = string(l[7:])
	case bytes.HasPrefix(l, []byte("data: ")) && t.event == "done":
		var d struct {
			Prompt    int `json:"prompt_tokens"`
			Completed int `json:"completion_tokens"`
			Searches  int `json:"web_searches"`
		}
		if json.Unmarshal(l[6:], &d) == nil {
			t.prompt, t.completed, t.searches, t.seen = d.Prompt, d.Completed, d.Searches, true
		}
	case len(l) == 0:
		t.event = ""
	}
}

func (s *Server) meterDone(userID string, t *doneTap) {
	if !t.seen {
		return
	}
	s.meterAdd(userID, "how.tokens", float64(t.prompt+t.completed)/1000)
	s.meterAdd(userID, "how.websearch", float64(t.searches))
}

func (s *Server) proxyOwn(ctx *fasthttp.RequestCtx, b business, u *User, up upstream, meter string) error {
	base, err := url.Parse(up.base)
	if err != nil {
		return err
	}
	req, resp := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
	ctx.Request.CopyTo(req)
	req.Header.DisableNormalizing()
	stripClientMojoHeaders(req)
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Del("Cookie")
	req.SetRequestURI(base.Scheme + "://" + base.Host + string(ctx.RequestURI()))
	req.Header.SetHost(base.Host)
	req.Header.Set("X-Mojo-Business", b.Slug)
	req.Header.Set("X-Mojo-Tenant", u.ID)
	req.Header.Set("X-Mojo-Token", up.token)
	if err := streamClient.Do(req, resp); err != nil {
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
		return err
	}
	fasthttp.ReleaseRequest(req)
	ok := resp.StatusCode() < 400
	ctx.SetStatusCode(resp.StatusCode())
	ctx.SetContentTypeBytes(resp.Header.ContentType())
	if ra := resp.Header.Peek("Retry-After"); len(ra) > 0 {
		ctx.Response.Header.SetBytesV("Retry-After", ra)
	}
	if !bytes.HasPrefix(bytes.ToLower(resp.Header.ContentType()), []byte("text/event-stream")) {
		body := append([]byte(nil), resp.Body()...)
		units := string(resp.Header.Peek("X-Mojo-Units"))
		fasthttp.ReleaseResponse(resp)
		ctx.SetBody(body)
		if ok {
			if modelMeter(b.Parts) != "" {
				s.meterUnits(u.ID, b, units)
			} else {
				s.meterAdd(u.ID, meter, 1)
			}
		}
		return nil
	}
	if ok {
		s.meterAdd(u.ID, meter, 1)
	}
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("X-Accel-Buffering", "no")
	maxDur, maxBytes := streamLimits()
	userID := u.ID
	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer fasthttp.ReleaseResponse(resp)
		defer resp.CloseBodyStream()
		tap := &doneTap{}
		defer func() { s.meterDone(userID, tap) }()
		deadline := time.Now().Add(maxDur)
		var total int64
		buf := make([]byte, 8192)
		src := resp.BodyStream()
		for time.Now().Before(deadline) && total < maxBytes {
			n, err := src.Read(buf)
			if n > 0 {
				total += int64(n)
				tap.feed(buf[:n])
				if _, werr := w.Write(buf[:n]); werr != nil || w.Flush() != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					return
				}
				return
			}
		}
	})
	return nil
}

func stripClientMojoHeaders(req *fasthttp.Request) {
	var drop []string
	req.Header.VisitAll(func(k, _ []byte) {
		if strings.HasPrefix(strings.ToLower(string(k)), "x-mojo-") {
			drop = append(drop, string(k))
		}
	})
	for _, k := range drop {
		req.Header.Del(k)
	}
}

// parseUnits reads the upstream's "meter=qty,meter=qty" usage report. Only
// meters the business owns are honoured, so a compromised or buggy upstream
// cannot bill a tenant for another product, and quantities must be finite and
// non-negative.
func parseUnits(h string, allowed []meterDef) map[string]float64 {
	out := map[string]float64{}
	for _, kv := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		q, err := strconv.ParseFloat(v, 64)
		if err != nil || q <= 0 || q > 1e9 || q != q {
			continue
		}
		for _, m := range allowed {
			if m.ID == k {
				out[k] += q
			}
		}
	}
	return out
}

func (s *Server) meterUnits(userID string, b business, header string) {
	for id, q := range parseUnits(header, metersFor(b.Parts)) {
		s.meterAdd(userID, id, q)
	}
}
