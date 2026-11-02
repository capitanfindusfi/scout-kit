//go:build integration

package browser

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// TestRemoteProfileKeepsCookiesAcrossRestarts needs a browserless service whose
// /profiles is writable, e.g.
//
//	docker run --rm -p 3999:3000 -e TIMEOUT=86400000 -v <volume owned by 999>:/profiles ghcr.io/browserless/chromium
//	BROWSER_TEST_WS_URL=ws://localhost:3999 go test -tags integration -run Remote ./internal/browser
//
// The browser reaches the test site on this machine as BROWSER_TEST_SITE_HOST
// (default host.docker.internal).
func TestRemoteProfileKeepsCookiesAcrossRestarts(t *testing.T) {
	wsURL := os.Getenv("BROWSER_TEST_WS_URL")
	if wsURL == "" {
		t.Skip("BROWSER_TEST_WS_URL not set")
	}
	siteHost := os.Getenv("BROWSER_TEST_SITE_HOST")
	if siteHost == "" {
		siteHost = "host.docker.internal"
	}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "refreshed", Value: "by-the-site", Path: "/", MaxAge: 3600})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	siteURL := fmt.Sprintf("http://%s:%d/", siteHost, listener.Addr().(*net.TCPAddr).Port)

	profile := fmt.Sprintf("/profiles/integration-%d", time.Now().UnixNano())
	state := t.TempDir()
	seed := []*proto.NetworkCookieParam{{Name: "starting", Value: "mine", Domain: siteHost, Path: "/"}}
	const raw = "starting=mine"

	// First run: the seed is applied and the site sets its own cookie
	b, err := NewRemote(wsURL, profile, state)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	page, err := b.NewPage(true, seed, raw)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	if err := page.Navigate(siteURL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	_ = page.WaitLoad()
	if got := cookieValue(t, b, page, "refreshed"); got != "by-the-site" {
		t.Fatalf("first run: refreshed = %q", got)
	}
	if ua := page.MustEval(`() => navigator.userAgent`).String(); strings.Contains(ua, "HeadlessChrome") {
		t.Errorf("the remote Chrome runs headless: %s", ua)
	}
	_ = b.Close()
	time.Sleep(time.Second)

	// Second run: the cookies set in the first one are still in the profile
	b, err = NewRemote(wsURL, profile, state)
	if err != nil {
		t.Fatalf("NewRemote (second run): %v", err)
	}
	defer b.Close()
	page, err = b.NewPage(true, seed, raw)
	if err != nil {
		t.Fatalf("NewPage (second run): %v", err)
	}
	if got := cookieValue(t, b, page, "refreshed"); got != "by-the-site" {
		t.Errorf("second run: refreshed = %q, want the cookie to survive the restart", got)
	}
	if got := cookieValue(t, b, page, "starting"); got != "mine" {
		t.Errorf("second run: starting = %q, want mine", got)
	}
}
