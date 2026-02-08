package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type roundTripperStub struct{}

func (roundTripperStub) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

func TestNewProxyAwareHTTPClient_ReusesProxyTransport(t *testing.T) {
	resetSharedTransportPoolForTests()
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ProxyURL: "http://127.0.0.1:18080",
		},
	}

	clientA := newProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	clientB := newProxyAwareHTTPClient(context.Background(), cfg, nil, 0)

	transportA, okA := clientA.Transport.(*http.Transport)
	transportB, okB := clientB.Transport.(*http.Transport)
	if !okA || !okB {
		t.Fatalf("expected pooled *http.Transport, got %T and %T", clientA.Transport, clientB.Transport)
	}
	if transportA != transportB {
		t.Fatal("expected same pooled transport instance for identical proxy settings")
	}
}

func TestNewProxyAwareHTTPClient_AuthProxyOverridesConfigProxy(t *testing.T) {
	resetSharedTransportPoolForTests()
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ProxyURL: "http://127.0.0.1:18080",
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "http://127.0.0.1:28080"}

	client := newProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.Transport)
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy resolver error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "127.0.0.1:28080" {
		t.Fatalf("expected auth proxy host 127.0.0.1:28080, got %#v", proxyURL)
	}
}

func TestNewProxyAwareHTTPClient_UsesContextRoundTripperWhenNoProxy(t *testing.T) {
	resetSharedTransportPoolForTests()
	rt := roundTripperStub{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))

	client := newProxyAwareHTTPClient(ctx, &config.Config{}, nil, 0)
	if client.Transport == nil {
		t.Fatal("expected non-nil transport")
	}
	if client.Transport != http.RoundTripper(rt) {
		t.Fatalf("expected context roundtripper to be used, got %T", client.Transport)
	}
}

func TestNewProxyAwareHTTPClient_ReusesDirectTransportWithoutProxy(t *testing.T) {
	resetSharedTransportPoolForTests()
	cfg := &config.Config{}

	clientA := newProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	clientB := newProxyAwareHTTPClient(context.Background(), cfg, nil, 0)

	transportA, okA := clientA.Transport.(*http.Transport)
	transportB, okB := clientB.Transport.(*http.Transport)
	if !okA || !okB {
		t.Fatalf("expected pooled *http.Transport, got %T and %T", clientA.Transport, clientB.Transport)
	}
	if transportA != transportB {
		t.Fatal("expected same pooled direct transport for identical settings")
	}
}
