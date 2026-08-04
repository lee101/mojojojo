package main

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/valyala/fasthttp"
)

func TestCachedBundleBuildsOnceUnderConcurrency(t *testing.T) {
	var bundle cachedBundle
	var builds atomic.Int32
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			body, etag, err := bundle.load(func() ([]byte, error) {
				builds.Add(1)
				return []byte("archive"), nil
			})
			if err != nil || string(body) != "archive" || etag == "" {
				t.Errorf("load = %q, %q, %v", body, etag, err)
			}
		}()
	}
	wait.Wait()
	if got := builds.Load(); got != 1 {
		t.Fatalf("bundle built %d times, want 1", got)
	}
}

func TestCachedBundleHonorsETag(t *testing.T) {
	first := &fasthttp.RequestCtx{}
	serveCachedBundle(first, []byte("archive"), `"digest"`)
	if first.Response.StatusCode() != fasthttp.StatusOK || string(first.Response.Body()) != "archive" {
		t.Fatalf("initial response = %d %q", first.Response.StatusCode(), first.Response.Body())
	}
	if got := string(first.Response.Header.Peek("Cache-Control")); got != "public, max-age=300" {
		t.Fatalf("cache control = %q", got)
	}

	cached := &fasthttp.RequestCtx{}
	cached.Request.Header.Set("If-None-Match", `"digest"`)
	serveCachedBundle(cached, []byte("archive"), `"digest"`)
	if cached.Response.StatusCode() != fasthttp.StatusNotModified || len(cached.Response.Body()) != 0 {
		t.Fatalf("cached response = %d %q", cached.Response.StatusCode(), cached.Response.Body())
	}
}
