// Package fetch gets pages over plain HTTP while looking like Chrome (TLS and
// HTTP/2 fingerprint included), which is enough for many sites that refuse
// ordinary Go clients. Hybrid adds a fallback, normally a real browser, for the
// requests a site still blocks.
package fetch

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	defaultTimeout = 30 * time.Second
	// DefaultUserAgent matches the TLS profile the client impersonates. A site
	// compares the two, so change them together.
	DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
	defaultLanguage  = "it-IT,it;q=0.9"
)

// Response is a fetched page.
type Response struct {
	Status int
	// URL is the final URL, after redirects.
	URL    string
	Header http.Header
	Body   []byte
	// Via says who fetched it: "http" or, for a Hybrid fallback, "browser".
	Via string
}

func (r *Response) Text() string { return string(r.Body) }

// Config tunes a Client. The zero value is fine.
type Config struct {
	Timeout        time.Duration
	UserAgent      string
	AcceptLanguage string
	// Proxy is an optional proxy URL.
	Proxy string
}

// Client is an HTTP client that impersonates Chrome and keeps cookies between
// requests. It is safe for concurrent use.
type Client struct {
	http tlsclient.HttpClient
	cfg  Config
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	if cfg.AcceptLanguage == "" {
		cfg.AcceptLanguage = defaultLanguage
	}

	options := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutSeconds(int(cfg.Timeout.Seconds())),
		tlsclient.WithClientProfile(profiles.Chrome_133),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
		tlsclient.WithRandomTLSExtensionOrder(),
	}
	if cfg.Proxy != "" {
		options = append(options, tlsclient.WithProxyUrl(cfg.Proxy))
	}
	c, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("failed to create http client: %w", err)
	}
	return &Client{http: c, cfg: cfg}, nil
}

// Get fetches rawURL as a browser navigation would. extra headers are added to
// (or override) the defaults. A 4xx or 5xx status is not an error: the caller
// decides, see Blocked.
func (c *Client) Get(ctx context.Context, rawURL string, extra map[string]string) (*Response, error) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}

	headers := map[string]string{
		"accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"accept-language": c.cfg.AcceptLanguage,
		"user-agent":      c.cfg.UserAgent,
	}
	for k, v := range extra {
		headers[strings.ToLower(k)] = v
	}
	req.Header = fhttp.Header{}
	order := make([]string, 0, len(headers))
	for _, k := range []string{"accept", "accept-language", "user-agent"} {
		order = append(order, k)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
		if k != "accept" && k != "accept-language" && k != "user-agent" {
			order = append(order, k)
		}
	}
	req.Header[fhttp.HeaderOrderKey] = order

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", rawURL, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", rawURL, err)
	}

	final := rawURL
	if res.Request != nil && res.Request.URL != nil {
		final = res.Request.URL.String()
	}
	header := make(http.Header, len(res.Header))
	for k, v := range res.Header {
		if k != fhttp.HeaderOrderKey && k != fhttp.PHeaderOrderKey {
			header[k] = v
		}
	}
	return &Response{Status: res.StatusCode, URL: final, Header: header, Body: body, Via: "http"}, nil
}

// Cookie is a cookie to hand over between the HTTP client and a browser.
type Cookie struct {
	Name, Value, Domain, Path string
	Secure                    bool
}

// SetCookies adds cookies to the client's jar. Domain is how a browser reports
// it (".example.com" for a domain cookie).
func (c *Client) SetCookies(cookies []Cookie) {
	byHost := map[string][]*fhttp.Cookie{}
	for _, ck := range cookies {
		host := strings.TrimPrefix(ck.Domain, ".")
		if host == "" || ck.Name == "" {
			continue
		}
		path := ck.Path
		if path == "" {
			path = "/"
		}
		byHost[host] = append(byHost[host], &fhttp.Cookie{
			Name: ck.Name, Value: ck.Value, Domain: ck.Domain, Path: path, Secure: ck.Secure,
		})
	}
	for host, list := range byHost {
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		c.http.SetCookies(u, list)
	}
}

// SeedCookies sets cookies given as a Cookie header ("name=value; other=value",
// as copied from a browser) for domain, e.g. ".immobiliare.it". Cookies Go
// rejects are skipped.
func (c *Client) SeedCookies(raw, domain string) error {
	var cookies []Cookie
	for pair := range strings.SplitSeq(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parsed, err := http.ParseCookie(pair)
		if err != nil {
			name, _, _ := strings.Cut(pair, "=")
			log.Printf("skipping cookie %q for %s: %v", name, domain, err)
			continue
		}
		for _, p := range parsed {
			cookies = append(cookies, Cookie{Name: p.Name, Value: p.Value, Domain: domain, Path: "/", Secure: true})
		}
	}
	if len(cookies) == 0 {
		return fmt.Errorf("no valid cookies for %s", domain)
	}
	c.SetCookies(cookies)
	return nil
}

// CookieHeader returns the Cookie header the client would send to rawURL.
func (c *Client) CookieHeader(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	pairs := []string{}
	for _, ck := range c.http.GetCookies(u) {
		pairs = append(pairs, ck.Name+"="+ck.Value)
	}
	return strings.Join(pairs, "; ")
}
