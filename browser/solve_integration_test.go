//go:build integration

package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// TestSolveChallenge stands in for a person: the "captcha" page sets a new
// cookie and redirects to the real page by itself after a few seconds. It needs
// a Chrome on the machine: run it with `go test -tags integration ./browser`.
func TestSolveChallenge(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/solved" {
			_, _ = w.Write([]byte("<html><body>the real page</body></html>"))
			return
		}
		_, _ = w.Write([]byte(`<html><body><iframe src="https://geo.captcha-delivery.com/captcha/"></iframe>
<script>setTimeout(() => {
  document.cookie = "datadome=fresh; path=/";
  location.replace("/solved");
}, 3000);</script></body></html>`))
	}))
	defer site.Close()

	profile := t.TempDir()
	b, err := New(profile)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cookies, err := b.SolveChallenge(ctx, site.URL)
	if err != nil {
		t.Fatalf("SolveChallenge: %v", err)
	}

	var got string
	for _, c := range cookies {
		if c.Name == "datadome" {
			got = c.Value
		}
	}
	if got != "fresh" {
		t.Errorf("datadome = %q, want fresh; cookies: %+v", got, cookies)
	}
}

func TestSolveChallengeTimesOut(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><iframe src="https://geo.captcha-delivery.com/captcha/"></iframe></body></html>`))
	}))
	defer site.Close()

	b, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := b.SolveChallenge(ctx, site.URL); !errors.Is(err, ErrChallengeUnsolved) {
		t.Errorf("err = %v, want ErrChallengeUnsolved", err)
	}
}

// TestSolveChallengeHook has the hook click the button that, on this page, is
// the "solution": the click is a real mouse event at the button's position.
func TestSolveChallengeHook(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/solved" {
			_, _ = w.Write([]byte("<html><body>the real page</body></html>"))
			return
		}
		_, _ = w.Write([]byte(`<html><body><iframe src="https://geo.captcha-delivery.com/captcha/"></iframe>
<button id="go" style="position:absolute;left:100px;top:200px;width:80px;height:40px"
  onmousedown="document.cookie='datadome=clicked; path=/'; location.replace('/solved')">go</button>
</body></html>`))
	}))
	defer site.Close()

	b, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()

	hook := func(ctx context.Context, page *rod.Page) error {
		button, err := page.Element("#go")
		if err != nil {
			return err
		}
		if err := button.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return err
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cookies, err := b.SolveChallenge(ctx, site.URL, WithPageHook(hook))
	if err != nil {
		t.Fatalf("SolveChallenge: %v", err)
	}
	for _, c := range cookies {
		if c.Name == "datadome" && c.Value == "clicked" {
			return
		}
	}
	t.Errorf("no datadome=clicked cookie: %+v", cookies)
}
