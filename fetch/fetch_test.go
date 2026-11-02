package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	cases := []struct {
		name string
		res  Response
		want bool
	}{
		{"ok", Response{Status: 200, Body: []byte("<html>fine</html>")}, false},
		{"forbidden", Response{Status: 403}, true},
		{"too many", Response{Status: 429}, true},
		{"not found is not a block", Response{Status: 404, Body: []byte("gone")}, false},
		{"datadome challenge on 200", Response{Status: 200, Body: []byte(`<script src="https://geo.captcha-delivery.com/x">`)}, true},
		{"cloudflare 503", Response{Status: 503, Body: []byte("<title>Just a moment...</title>")}, true},
		{"plain 503", Response{Status: 503, Body: []byte("maintenance")}, false},
		{"big page mentioning datadome", Response{Status: 200, Body: []byte(strings.Repeat("x", maxChallengeBody) + "captcha-delivery.com")}, false},
	}
	for _, c := range cases {
		if got := Blocked(&c.res); got != c.want {
			t.Errorf("%s: Blocked = %v, want %v", c.name, got, c.want)
		}
	}
}

func newClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(Config{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientGetSendsBrowserHeadersAndKeepsCookies(t *testing.T) {
	var ua, cookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua, cookie = r.Header.Get("User-Agent"), r.Header.Get("Cookie")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc", Path: "/"})
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	c := newClient(t)
	res, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil || res.Status != 200 || res.Text() != "hello" || res.Via != "http" {
		t.Fatalf("Get = %+v, %v", res, err)
	}
	if ua != DefaultUserAgent {
		t.Errorf("user agent = %q", ua)
	}
	if _, err := c.Get(context.Background(), srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cookie, "session=abc") {
		t.Errorf("second request cookie = %q, want the session cookie back", cookie)
	}
}

func TestSeedCookies(t *testing.T) {
	c := newClient(t)
	if err := c.SeedCookies("a=1; b=2; ; c=3", ".example.com"); err != nil {
		t.Fatal(err)
	}
	got := c.CookieHeader("https://www.example.com/x")
	for _, want := range []string{"a=1", "b=2", "c=3"} {
		if !strings.Contains(got, want) {
			t.Errorf("CookieHeader = %q, missing %s", got, want)
		}
	}
	if err := c.SeedCookies("   ", ".example.com"); err == nil {
		t.Error("empty seed: want error")
	}
}

// blockingServer refuses requests without the "pass" cookie.
func blockingServer(hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if ck, err := r.Cookie("pass"); err != nil || ck.Value != "yes" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("real page"))
	}))
}

func TestHybridFallsBackThenRetriesWithBrowserCookies(t *testing.T) {
	var hits atomic.Int32
	srv := blockingServer(&hits)
	defer srv.Close()

	// The jar keys cookies by host including the port, which real sites never have, so the test cookie's
	// domain carries it.
	target := srv.URL
	cookieDomain := strings.TrimPrefix(srv.URL, "http://")

	var fallbackCalls int
	now := time.Now()
	h := NewHybrid(newClient(t), func(_ context.Context, u string) (*Response, []Cookie, error) {
		fallbackCalls++
		return &Response{Status: 200, URL: u, Body: []byte("from browser")},
			[]Cookie{{Name: "pass", Value: "yes", Domain: cookieDomain, Path: "/"}}, nil
	})
	h.Cooldown = time.Minute
	h.now = func() time.Time { return now }

	res, err := h.Get(context.Background(), target, nil)
	if err != nil || res.Via != "browser" || res.Text() != "from browser" {
		t.Fatalf("first Get = %+v, %v", res, err)
	}

	// During the cooldown the blocked HTTP route is not tried again.
	before := hits.Load()
	res, err = h.Get(context.Background(), target, nil)
	if err != nil || res.Via != "browser" || hits.Load() != before || fallbackCalls != 2 {
		t.Fatalf("cooldown Get = %+v, %v (hits %d->%d, fallback calls %d)", res, err, before, hits.Load(), fallbackCalls)
	}

	// After it, HTTP works again thanks to the browser's cookie.
	now = now.Add(2 * time.Minute)
	res, err = h.Get(context.Background(), target, nil)
	if err != nil || res.Via != "http" || res.Text() != "real page" || fallbackCalls != 2 {
		t.Fatalf("after cooldown Get = %+v, %v (fallback calls %d)", res, err, fallbackCalls)
	}
}

func TestHybridWithoutFallbackReportsBlocked(t *testing.T) {
	var hits atomic.Int32
	srv := blockingServer(&hits)
	defer srv.Close()

	res, err := NewHybrid(newClient(t), nil).Get(context.Background(), srv.URL, nil)
	if !errors.Is(err, ErrBlocked) || res == nil || res.Status != 403 {
		t.Fatalf("Get = %+v, %v; want ErrBlocked with the 403 response", res, err)
	}
}

func TestHybridFallbackStillBlocked(t *testing.T) {
	var hits atomic.Int32
	srv := blockingServer(&hits)
	defer srv.Close()

	h := NewHybrid(newClient(t), func(_ context.Context, u string) (*Response, []Cookie, error) {
		return &Response{Status: 403, URL: u}, nil, nil
	})
	if _, err := h.Get(context.Background(), srv.URL, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
}

func TestHybridPassesThroughWhenNotBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	h := NewHybrid(newClient(t), func(context.Context, string) (*Response, []Cookie, error) {
		t.Error("fallback used for a plain 404")
		return nil, nil, errors.New("unexpected")
	})
	res, err := h.Get(context.Background(), srv.URL, nil)
	if err != nil || res.Status != 404 {
		t.Fatalf("Get = %+v, %v; a 404 goes back to the caller", res, err)
	}
}
