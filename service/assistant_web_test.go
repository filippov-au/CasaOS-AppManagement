package service

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestAssistantWebSearchParsesDirectAndRedirectLinks(t *testing.T) {
	base, _ := url.Parse("https://html.duckduckgo.com/html/")
	body := []byte(`<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fbook.epub">A <b>book</b></a><a class="result__a" href="https://example.com/data">Data</a><a class="result__a" href="file:///etc/passwd">unsafe</a><a href="/settings">Settings</a>`)
	links, err := assistantSearchResults(body, base)
	if err != nil || len(links) != 2 || links[0].URL != "https://example.org/book.epub" || links[0].Title != "A book" {
		t.Fatal(links, err)
	}
	if _, err := assistantSearchResults([]byte(`<form id="challenge-form">CAPTCHA</form>`), base); err == nil {
		t.Fatal("challenge reported as empty success")
	}
	if links, err := assistantSearchResults([]byte(`<p>No results found</p>`), base); err != nil || len(links) != 0 {
		t.Fatal(links, err)
	}
}

func TestAssistantWebSearchFallsBackWithoutBypassingChallenge(t *testing.T) {
	calls := []string{}
	client := &http.Client{Transport: contentRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Hostname())
		status, body := 202, `<html>Challenge</html>`
		if r.URL.Hostname() == "www.bing.com" {
			status = 200
			body = `<rss><channel><item><title>Book data</title><link>https://example.org/book.mobi</link></item><item><title>unsafe</title><link>http://127.0.0.1/</link></item></channel></rss>`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})}
	result, err := assistantSearchPublic(context.Background(), client, "ebook data")
	if err != nil || len(calls) != 2 || !strings.Contains(result, `"fallback_used": true`) || !strings.Contains(result, "https://example.org/book.mobi") || strings.Contains(result, "127.0.0.1") {
		t.Fatal(result, calls, err)
	}
	if _, err := assistantSearchRSS([]byte(`<html>Sign in</html>`)); err == nil {
		t.Fatal("accepted non-RSS")
	}
}

func TestAssistantWebPageReturnsTextAndPublicLinks(t *testing.T) {
	base, _ := url.Parse("https://example.com/books/")
	text, links := assistantWebPage([]byte(`<html><script>stealSecrets()</script><style>.hidden{}</style><p>Download book</p><a href="novel.mobi">MOBI</a><a href="http://127.0.0.1/">local</a><a href="javascript:alert(1)">script</a></html>`), base)
	if strings.Contains(text, "stealSecrets") || strings.Contains(text, ".hidden") || !strings.Contains(text, "Download book") || len(links) != 1 || links[0].URL != "https://example.com/books/novel.mobi" {
		t.Fatal(text, links)
	}
	for _, raw := range []string{`{"url":"file:///etc/passwd"}`, `{"url":"https://example.com/", "offset":-1}`, `{"url":"https://example.com/", "headers":{"Authorization":"secret"}}`} {
		if _, err := assistantWebPlan("web_read", []byte(raw)); err == nil {
			t.Fatal("unsafe web arguments accepted", raw)
		}
	}
}

func TestAssistantWebLive(t *testing.T) {
	if os.Getenv("CASAOS_ASSISTANT_WEB_LIVE_TEST") != "1" {
		t.Skip("set CASAOS_ASSISTANT_WEB_LIVE_TEST=1 for public web smoke test")
	}
	for _, test := range []struct{ name, args string }{
		{"web_search", `{"query":"calibre ebook documentation"}`},
		{"web_read", `{"url":"https://manual.calibre-ebook.com/"}`},
	} {
		p, err := assistantWebPlan(test.name, []byte(test.args))
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.Execute(context.Background())
		if err != nil {
			t.Fatal(test.name, err)
		}
		if !strings.Contains(strings.ToLower(result), "calibre") {
			t.Fatal("no relevant web evidence", result)
		}
		t.Logf("%s returned %d bytes of public web evidence", test.name, len(result))
	}
}
