package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestVersionGreater(t *testing.T) {
	if !versionGreater("1.2.0", "1.1.9") {
		t.Fatal("expected newer")
	}
	if versionGreater("1.1.0", "1.1.0") {
		t.Fatal("expected equal")
	}
	if versionGreater("0.9.9", "1.0.0") {
		t.Fatal("expected older")
	}
}

func TestSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: "1.2.3", Asset: "https://example/gateway.exe", SHA256: "abcdef"}
	msg := []byte(m.Version + "\n" + m.SHA256 + "\n" + m.Asset)
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
	if err := verifyManifest(m, base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	m.SHA256 = "ffffff"
	if err := verifyManifest(m, base64.StdEncoding.EncodeToString(pub)); err == nil {
		t.Fatal("expected signature failure")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProxyFallbackTransportUsesDirectWhenProxyDown(t *testing.T) {
	proxied := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: "https://example/latest.json",
			Err: errors.New("proxyconnect tcp: dial tcp 127.0.0.1:10808: connectex: No connection could be made")}
	})
	directCalled := false
	direct := roundTripFunc(func(*http.Request) (*http.Response, error) {
		directCalled = true
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	tr := &proxyFallbackTransport{proxied: proxied, direct: direct}
	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "https://example/latest.json", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !directCalled || resp.StatusCode != 200 {
		t.Fatalf("fallback not used: called=%v status=%d", directCalled, resp.StatusCode)
	}
}

func TestProxyFallbackTransportKeepsNonProxyErrors(t *testing.T) {
	directCalled := false
	proxied := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: "https://example/latest.json",
			Err: errors.New("dial tcp 1.2.3.4:443: i/o timeout")}
	})
	direct := roundTripFunc(func(*http.Request) (*http.Response, error) {
		directCalled = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	tr := &proxyFallbackTransport{proxied: proxied, direct: direct}
	if _, err := tr.RoundTrip(httptest.NewRequest("GET", "https://example/latest.json", nil)); err == nil {
		t.Fatal("expected upstream error")
	}
	if directCalled {
		t.Fatal("direct must not be used for non-proxy errors")
	}
}

func TestIsProxyDialError(t *testing.T) {
	if !isProxyDialError(&url.Error{Err: errors.New("proxyconnect tcp: dial tcp 127.0.0.1:10808: connectex: refused")}) {
		t.Fatal("expected proxyconnect error")
	}
	if isProxyDialError(errors.New("dial tcp 1.2.3.4:443: i/o timeout")) {
		t.Fatal("unexpected match")
	}
}

func TestProxyFallbackTransportReportsBothFailures(t *testing.T) {
	proxied := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Err: errors.New("proxyconnect tcp: dial tcp 127.0.0.1:10808: refused")}
	})
	direct := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 57.155.223.240:443: i/o timeout")
	})
	tr := &proxyFallbackTransport{proxied: proxied, direct: direct}
	_, err := tr.RoundTrip(httptest.NewRequest("GET", "https://example/latest.json", nil))
	if err == nil || !strings.Contains(err.Error(), "proxy unavailable") || !strings.Contains(err.Error(), "direct retry failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}
