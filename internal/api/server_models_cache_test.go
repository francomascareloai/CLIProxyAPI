package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func runModelsRequestRaw(server *Server, userAgent string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	return rr
}

func runModelsRequest(t *testing.T, server *Server, userAgent string) *httptest.ResponseRecorder {
	t.Helper()
	rr := runModelsRequestRaw(server, userAgent)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d body=%s", rr.Code, rr.Body.String())
	}
	return rr
}

func TestUnifiedModelsHandlerCachesPerUserAgent(t *testing.T) {
	server := newTestServer(t)

	openAIFirst := runModelsRequest(t, server, "curl/8.0")
	if got := server.modelsCacheMiss.Load(); got != 1 {
		t.Fatalf("expected 1 cache miss after first openai request, got %d", got)
	}
	if got := server.modelsCacheHit.Load(); got != 0 {
		t.Fatalf("expected 0 cache hits after first openai request, got %d", got)
	}

	openAISecond := runModelsRequest(t, server, "curl/8.0")
	if got := server.modelsCacheMiss.Load(); got != 1 {
		t.Fatalf("expected openai second request to hit cache, misses=%d", got)
	}
	if got := server.modelsCacheHit.Load(); got != 1 {
		t.Fatalf("expected 1 cache hit after openai second request, got %d", got)
	}
	if openAIFirst.Body.String() != openAISecond.Body.String() {
		t.Fatalf("cached openai models response mismatch")
	}

	claudeFirst := runModelsRequest(t, server, "claude-cli/1.2.3")
	if got := server.modelsCacheMiss.Load(); got != 2 {
		t.Fatalf("expected first claude request to miss dedicated cache bucket, misses=%d", got)
	}

	claudeSecond := runModelsRequest(t, server, "claude-cli/1.2.3")
	if got := server.modelsCacheHit.Load(); got != 2 {
		t.Fatalf("expected second claude request to hit cache, hits=%d", got)
	}
	if claudeFirst.Body.String() != claudeSecond.Body.String() {
		t.Fatalf("cached claude models response mismatch")
	}
}

func TestModelsCacheInvalidatedOnAuthSnapshotUpdate(t *testing.T) {
	server := newTestServer(t)
	initialVersion := server.modelsCacheVersion.Load()

	runModelsRequest(t, server, "curl/8.0")
	runModelsRequest(t, server, "curl/8.0")
	if got := server.modelsCacheHit.Load(); got != 1 {
		t.Fatalf("expected cache hit before invalidation, got %d", got)
	}
	if got := server.modelsCacheMiss.Load(); got != 1 {
		t.Fatalf("expected cache miss count 1 before invalidation, got %d", got)
	}

	server.UpdateAuthSnapshot(server.cfg, 0)
	if got := server.modelsCacheInvalidated.Load(); got != 1 {
		t.Fatalf("expected one cache invalidation, got %d", got)
	}
	if got := server.modelsCacheVersion.Load(); got != initialVersion+1 {
		t.Fatalf("expected cache version %d after invalidation, got %d", initialVersion+1, got)
	}
	runModelsRequest(t, server, "curl/8.0")

	if got := server.modelsCacheMiss.Load(); got != 2 {
		t.Fatalf("expected cache miss after invalidation, got %d", got)
	}
}

func TestUnifiedModelsHandlerSingleflightBuildOnConcurrentMiss(t *testing.T) {
	server := newTestServer(t)
	var buildCalls atomic.Int32
	server.modelsBuildHook = func() {
		buildCalls.Add(1)
		time.Sleep(40 * time.Millisecond)
	}

	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rr := runModelsRequestRaw(server, "curl/8.0")
			if rr.Code != http.StatusOK {
				errs <- rr.Body.String()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for body := range errs {
		t.Fatalf("unexpected non-200 response body=%s", body)
	}

	if got := buildCalls.Load(); got != 1 {
		t.Fatalf("expected single models payload build under concurrent miss, got %d", got)
	}
	if got := server.modelsCacheBuildCount.Load(); got != 1 {
		t.Fatalf("expected build counter 1 under concurrent miss, got %d", got)
	}
}
