package browser

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const testDomain = ".immobiliare.it"

func param(name, value string) *proto.NetworkCookieParam {
	return &proto.NetworkCookieParam{Name: name, Value: value, Domain: testDomain, Path: "/"}
}

func stored(name, value, domain string) *proto.NetworkCookie {
	return &proto.NetworkCookie{Name: name, Value: value, Domain: domain, Path: "/"}
}

func names[T any](items []T, name func(T) string) []string {
	var result []string
	for _, item := range items {
		result = append(result, name(item))
	}
	return result
}

func TestParseCookies(t *testing.T) {
	cookies, err := ParseCookies("datadome=abc; session=xyz;; ", testDomain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cookies) != 2 || cookies[0].Name != "datadome" || cookies[1].Value != "xyz" || cookies[0].Domain != testDomain {
		t.Errorf("cookies = %+v", cookies)
	}

	if cookies, err := ParseCookies("", testDomain); cookies != nil || err != nil {
		t.Errorf("empty seed = %v, %v; want nothing", cookies, err)
	}
	if _, err := ParseCookies("   ;  ", testDomain); err == nil {
		t.Error("expected an error when no cookie is valid")
	}
}

func TestCookiesToSetKeepsTheBrowsersCopyByDefault(t *testing.T) {
	existing := []*proto.NetworkCookie{stored("datadome", "refreshed", ".immobiliare.it")}
	seed := []*proto.NetworkCookieParam{param("datadome", "pasted"), param("session", "s")}

	toSet, toDelete := cookiesToSet(existing, seed, false)

	got := names(toSet, func(c *proto.NetworkCookieParam) string { return c.Name })
	if len(got) != 1 || got[0] != "session" || len(toDelete) != 0 {
		t.Errorf("toSet = %v, toDelete = %v; want only session set", got, toDelete)
	}
}

func TestCookiesToSetReplacesTheBrowsersCopyForAChangedSeed(t *testing.T) {
	existing := []*proto.NetworkCookie{
		stored("datadome", "refreshed", "www.immobiliare.it"),
		stored("other", "keep", ".immobiliare.it"),
		stored("datadome", "elsewhere", ".example.com"),
	}
	seed := []*proto.NetworkCookieParam{param("datadome", "pasted")}

	toSet, toDelete := cookiesToSet(existing, seed, true)

	if len(toSet) != 1 || toSet[0].Value != "pasted" {
		t.Errorf("toSet = %+v, want the pasted cookie", toSet)
	}
	if len(toDelete) != 1 || toDelete[0].Domain != "www.immobiliare.it" {
		t.Errorf("toDelete = %+v, want only the immobiliare datadome", toDelete)
	}
}

func TestSeedChangedIsRememberedPerDomain(t *testing.T) {
	dir := t.TempDir()
	const seed = "datadome=abc"

	if !seedChanged(dir, testDomain, seed) {
		t.Error("a seed never applied should count as changed")
	}
	if err := rememberSeed(dir, testDomain, seed); err != nil {
		t.Fatalf("rememberSeed: %v", err)
	}
	if seedChanged(dir, testDomain, seed) {
		t.Error("the applied seed should no longer count as changed")
	}
	if !seedChanged(dir, testDomain, "datadome=new") {
		t.Error("a different seed should count as changed")
	}
	if !seedChanged(dir, ".other.it", seed) {
		t.Error("another domain has its own seed")
	}
}

func TestClearProfileLocks(t *testing.T) {
	dir := t.TempDir()
	for _, name := range append(profileLockFiles, "Cookies") {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	clearProfileLocks(dir)
	// A second call, with the locks already gone, must be harmless
	clearProfileLocks(dir)

	for _, name := range profileLockFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err = %v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Cookies")); err != nil {
		t.Errorf("the profile's own files must stay: %v", err)
	}
}

func TestCleanupNeverRemovesAPersistentProfile(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "Cookies")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	b := &Browser{profileDir: dir}
	// The launcher never ran: if cleanup touched it, it would block or delete the dir
	b.cleanup(launcher.New().UserDataDir(dir))

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("persistent profile was removed: %v", err)
	}
}

func TestRemoteLaunchURL(t *testing.T) {
	got, err := remoteLaunchURL("ws://chromium:3000?token=secret", "/profiles/flat-scout")
	if err != nil {
		t.Fatalf("remoteLaunchURL: %v", err)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("not a URL: %v", err)
	}
	if u.Host != "chromium:3000" || u.Query().Get("token") != "secret" {
		t.Errorf("the base URL and its query must be kept, got %s", got)
	}

	raw, err := base64.StdEncoding.DecodeString(u.Query().Get("launch"))
	if err != nil {
		t.Fatalf("launch is not base64: %v", err)
	}
	var options struct {
		Headless *bool    `json:"headless"`
		Args     []string `json:"args"`
	}
	if err := json.Unmarshal(raw, &options); err != nil {
		t.Fatalf("launch is not JSON: %v", err)
	}

	if options.Headless == nil || *options.Headless {
		t.Error("the remote Chrome must have a window, like the local one")
	}
	want := []string{
		"--disable-blink-features=AutomationControlled",
		"--lang=it-IT",
		"--accept-lang=it-IT,it",
		"--user-data-dir=/profiles/flat-scout",
	}
	if !slices.Equal(options.Args, want) {
		t.Errorf("args = %v, want %v", options.Args, want)
	}
}

func TestRemoteLaunchURLWithoutProfile(t *testing.T) {
	got, err := remoteLaunchURL("ws://chromium:3000", "")
	if err != nil {
		t.Fatalf("remoteLaunchURL: %v", err)
	}
	u, _ := url.Parse(got)
	raw, _ := base64.StdEncoding.DecodeString(u.Query().Get("launch"))
	if strings.Contains(string(raw), "user-data-dir") {
		t.Errorf("no profile was given, but the launch options set one: %s", raw)
	}
}

func TestRemoteLaunchURLRejectsOtherSchemes(t *testing.T) {
	for _, bad := range []string{"http://chromium:3000", "chromium:3000", "://"} {
		if _, err := remoteLaunchURL(bad, ""); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestCookieHeader(t *testing.T) {
	cookies := []*proto.NetworkCookie{
		stored("datadome", "abc", testDomain),
		stored("IM_PAP_TI", "xyz==", testDomain),
	}

	if got := CookieHeader(cookies); got != "datadome=abc; IM_PAP_TI=xyz==" {
		t.Errorf("CookieHeader() = %q", got)
	}
	if got := CookieHeader(nil); got != "" {
		t.Errorf("CookieHeader(nil) = %q, want empty", got)
	}
}

func TestCookieAppliesTo(t *testing.T) {
	cases := []struct {
		domain, host string
		want         bool
	}{
		{".immobiliare.it", "www.immobiliare.it", true},
		{".immobiliare.it", "immobiliare.it", true},
		{"www.immobiliare.it", "www.immobiliare.it", true},
		{"www.immobiliare.it", "api.immobiliare.it", false},
		{".immobiliare.it", "evilimmobiliare.it", false},
		{".other.com", "www.immobiliare.it", false},
	}
	for _, c := range cases {
		if got := cookieAppliesTo(c.domain, c.host); got != c.want {
			t.Errorf("cookieAppliesTo(%q, %q) = %v, want %v", c.domain, c.host, got, c.want)
		}
	}
}
