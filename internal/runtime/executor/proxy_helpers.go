package executor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

type upstreamTransportSettings struct {
	maxIdleConns          int
	maxIdleConnsPerHost   int
	maxConnsPerHost       int
	idleConnTimeout       time.Duration
	tlsHandshakeTimeout   time.Duration
	responseHeaderTimeout time.Duration
	expectContinueTimeout time.Duration
	disableKeepAlives     bool
}

var sharedTransportPool sync.Map

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured
// 3. Use RoundTripper from context if neither are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}
	settings := upstreamTransportSettingsFromConfig(cfg)

	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := pooledProxyTransport(proxyURL, settings)
		if transport != nil {
			httpClient.Transport = transport
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
		return httpClient
	}

	// Fallback: pooled direct transport (keep-alive + shared socket reuse).
	httpClient.Transport = pooledProxyTransport("", settings)
	return httpClient
}

func upstreamTransportSettingsFromConfig(cfg *config.Config) upstreamTransportSettings {
	s := upstreamTransportSettings{
		maxIdleConns:          config.DefaultUpstreamMaxIdleConns,
		maxIdleConnsPerHost:   config.DefaultUpstreamIdlePerHost,
		maxConnsPerHost:       config.DefaultUpstreamMaxPerHost,
		idleConnTimeout:       time.Duration(config.DefaultUpstreamIdleTimeoutMS) * time.Millisecond,
		tlsHandshakeTimeout:   time.Duration(config.DefaultUpstreamTLSHSMS) * time.Millisecond,
		responseHeaderTimeout: time.Duration(config.DefaultUpstreamRespHdrMS) * time.Millisecond,
		expectContinueTimeout: time.Duration(config.DefaultUpstreamExpectContMS) * time.Millisecond,
	}
	if cfg == nil {
		return s
	}
	if cfg.UpstreamHTTP.MaxIdleConns > 0 {
		s.maxIdleConns = cfg.UpstreamHTTP.MaxIdleConns
	}
	if cfg.UpstreamHTTP.MaxIdleConnsPerHost > 0 {
		s.maxIdleConnsPerHost = cfg.UpstreamHTTP.MaxIdleConnsPerHost
	}
	if cfg.UpstreamHTTP.MaxConnsPerHost > 0 {
		s.maxConnsPerHost = cfg.UpstreamHTTP.MaxConnsPerHost
	}
	if cfg.UpstreamHTTP.IdleConnTimeoutMS > 0 {
		s.idleConnTimeout = time.Duration(cfg.UpstreamHTTP.IdleConnTimeoutMS) * time.Millisecond
	}
	if cfg.UpstreamHTTP.TLSHandshakeTimeoutMS > 0 {
		s.tlsHandshakeTimeout = time.Duration(cfg.UpstreamHTTP.TLSHandshakeTimeoutMS) * time.Millisecond
	}
	if cfg.UpstreamHTTP.ResponseHeaderTimeoutMS > 0 {
		s.responseHeaderTimeout = time.Duration(cfg.UpstreamHTTP.ResponseHeaderTimeoutMS) * time.Millisecond
	}
	if cfg.UpstreamHTTP.ExpectContinueTimeoutMS > 0 {
		s.expectContinueTimeout = time.Duration(cfg.UpstreamHTTP.ExpectContinueTimeoutMS) * time.Millisecond
	}
	s.disableKeepAlives = cfg.UpstreamHTTP.DisableKeepAlives
	return s
}

func pooledProxyTransport(proxyURL string, settings upstreamTransportSettings) *http.Transport {
	key := transportCacheKey(proxyURL, settings)
	if cached, ok := sharedTransportPool.Load(key); ok {
		if transport, okCast := cached.(*http.Transport); okCast && transport != nil {
			return transport
		}
	}

	transport := buildProxyTransport(proxyURL, settings)
	if transport == nil {
		return nil
	}
	actual, _ := sharedTransportPool.LoadOrStore(key, transport)
	if finalTransport, ok := actual.(*http.Transport); ok && finalTransport != nil {
		return finalTransport
	}
	return transport
}

func transportCacheKey(proxyURL string, settings upstreamTransportSettings) string {
	return fmt.Sprintf(
		"%s|%d|%d|%d|%d|%d|%d|%d|%t",
		strings.TrimSpace(proxyURL),
		settings.maxIdleConns,
		settings.maxIdleConnsPerHost,
		settings.maxConnsPerHost,
		settings.idleConnTimeout.Milliseconds(),
		settings.tlsHandshakeTimeout.Milliseconds(),
		settings.responseHeaderTimeout.Milliseconds(),
		settings.expectContinueTimeout.Milliseconds(),
		settings.disableKeepAlives,
	)
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string, settings upstreamTransportSettings) *http.Transport {
	baseTransport := &http.Transport{
		MaxIdleConns:          settings.maxIdleConns,
		MaxIdleConnsPerHost:   settings.maxIdleConnsPerHost,
		MaxConnsPerHost:       settings.maxConnsPerHost,
		IdleConnTimeout:       settings.idleConnTimeout,
		TLSHandshakeTimeout:   settings.tlsHandshakeTimeout,
		ResponseHeaderTimeout: settings.responseHeaderTimeout,
		ExpectContinueTimeout: settings.expectContinueTimeout,
		DisableKeepAlives:     settings.disableKeepAlives,
		ForceAttemptHTTP2:     true,
	}
	if strings.TrimSpace(proxyURL) == "" {
		return baseTransport
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("parse proxy URL failed: %v", errParse)
		return nil
	}

	var transport *http.Transport

	// Handle different proxy schemes
	if parsedURL.Scheme == "socks5" {
		// Configure SOCKS5 proxy with optional authentication
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
			return nil
		}
		// Set up a custom transport using the SOCKS5 dialer
		transport = baseTransport.Clone()
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	} else if parsedURL.Scheme == "http" || parsedURL.Scheme == "https" {
		// Configure HTTP or HTTPS proxy
		transport = baseTransport.Clone()
		transport.Proxy = http.ProxyURL(parsedURL)
	} else {
		log.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
		return nil
	}

	return transport
}

// resetSharedTransportPoolForTests clears shared transport cache.
func resetSharedTransportPoolForTests() {
	sharedTransportPool.Range(func(key, value any) bool {
		sharedTransportPool.Delete(key)
		return true
	})
}
