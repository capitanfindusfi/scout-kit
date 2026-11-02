//go:build integration

package browser

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// cookieValue opens the url and returns the value of the named cookie, or "".
func cookieValue(t *testing.T, b *Browser, page *rod.Page, name string) string {
	t.Helper()
	cookies, err := b.rod.GetCookies()
	if err != nil {
		t.Fatalf("GetCookies: %v", err)
	}
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// TestProfileKeepsCookiesAcrossRestarts needs a Chrome on the machine and opens
// a window: run it with `go test -tags integration ./internal/browser`.
func TestProfileKeepsCookiesAcrossRestarts(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "refreshed", Value: "by-the-site", Path: "/", MaxAge: 3600})
	}))
	defer site.Close()

	profile := t.TempDir()
	seed := []*proto.NetworkCookieParam{{Name: "starting", Value: "mine", Domain: "127.0.0.1", Path: "/"}}
	const raw = "starting=mine"

	// First run: the seed is applied and the site sets its own cookie
	b, err := New(profile)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	page, err := b.NewPage(true, seed, raw)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	if err := page.Navigate(site.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	_ = page.WaitLoad()
	if got := cookieValue(t, b, page, "refreshed"); got != "by-the-site" {
		t.Fatalf("first run: refreshed = %q", got)
	}
	_ = b.Close()
	time.Sleep(time.Second)

	// Second run, same seed: the site's cookie is still there
	b, err = New(profile)
	if err != nil {
		t.Fatalf("New (second run): %v", err)
	}
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
	_ = b.Close()
	time.Sleep(time.Second)

	// Third run, new seed: it replaces the profile's copy of the cookie
	newSeed := []*proto.NetworkCookieParam{{Name: "starting", Value: "fresh", Domain: "127.0.0.1", Path: "/"}}
	b, err = New(profile)
	if err != nil {
		t.Fatalf("New (third run): %v", err)
	}
	defer b.Close()
	page, err = b.NewPage(true, newSeed, "starting=fresh")
	if err != nil {
		t.Fatalf("NewPage (third run): %v", err)
	}
	_ = page
	if got := cookieValue(t, b, page, "starting"); got != "fresh" {
		t.Errorf("third run: starting = %q, want the new seed to replace it", got)
	}
}
