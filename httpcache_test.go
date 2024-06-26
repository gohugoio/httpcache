package httpcache

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

var s struct {
	server    *httptest.Server
	client    http.Client
	transport *Transport
	done      chan struct{} // Closed to unlock infinite handlers.
}

type fakeClock struct {
	elapsed time.Duration
}

func (c *fakeClock) since(t time.Time) time.Duration {
	return c.elapsed
}

func TestMain(m *testing.M) {
	flag.Parse()
	setup()
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup() {
	tp := newMemoryCacheTransport()
	client := http.Client{Transport: tp}
	s.transport = tp
	s.client = client
	s.done = make(chan struct{})

	mux := http.NewServeMux()
	s.server = httptest.NewServer(mux)

	mux.HandleFunc("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
	}))

	mux.HandleFunc("/method", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Write([]byte(r.Method))
	}))

	mux.HandleFunc("/helloheaderasbody", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("Hello")))
	}))

	mux.HandleFunc("/range", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lm := "Fri, 14 Dec 2010 01:01:50 GMT"
		if r.Header.Get("if-modified-since") == lm {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("last-modified", lm)
		if r.Header.Get("range") == "bytes=4-9" {
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(" text "))
			return
		}
		w.Write([]byte("Some text content"))
	}))

	mux.HandleFunc("/nostore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
	}))

	mux.HandleFunc("/etag", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := "124567"
		if r.Header.Get("if-none-match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("etag", etag)
	}))

	mux.HandleFunc("/lastmodified", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lm := "Fri, 14 Dec 2010 01:01:50 GMT"
		if r.Header.Get("if-modified-since") == lm {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("last-modified", lm)
	}))

	mux.HandleFunc("/varyaccept", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Vary", "Accept")
		w.Write([]byte("Some text content"))
	}))

	mux.HandleFunc("/doublevary", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Vary", "Accept, Accept-Language")
		w.Write([]byte("Some text content"))
	}))
	mux.HandleFunc("/2varyheaders", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Add("Vary", "Accept")
		w.Header().Add("Vary", "Accept-Language")
		w.Write([]byte("Some text content"))
	}))
	mux.HandleFunc("/varyunused", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Vary", "X-Madeup-Header")
		w.Write([]byte("Some text content"))
	}))

	mux.HandleFunc("/cachederror", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := "abc"
		if r.Header.Get("if-none-match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("etag", etag)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not found"))
	}))

	updateFieldsCounter := 0
	mux.HandleFunc("/updatefields", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Counter", strconv.Itoa(updateFieldsCounter))
		w.Header().Set("Etag", `"e"`)
		updateFieldsCounter++
		if r.Header.Get("if-none-match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte("Some text content"))
	}))

	// Take 3 seconds to return 200 OK (for testing client timeouts).
	mux.HandleFunc("/3seconds", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))

	mux.HandleFunc("/infinite", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			select {
			case <-s.done:
				return
			default:
				w.Write([]byte{0})
			}
		}
	}))
}

func teardown() {
	close(s.done)
	s.server.Close()
}

func cacheSize() int {
	return s.transport.Cache.(*memoryCache).Size()
}

func resetTest() {
	s.transport.Cache = newMemoryCache()
	s.transport.CacheKey = nil
	s.transport.AlwaysUseCachedResponse = nil
	s.transport.ShouldCache = nil
	s.transport.EnableETagPair = false
	s.transport.MarkCachedResponses = false
	clock = &realClock{}
}

// TestCacheableMethod ensures that uncacheable method does not get stored
// in cache and get incorrectly used for a following cacheable method request.
func TestCacheableMethod(t *testing.T) {
	resetTest()
	c := qt.New(t)
	{

		body, resp := doMethod(t, "POST", "/method", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(body, qt.Equals, "POST")
	}
	{
		body, resp := doMethod(t, "GET", "/method", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(body, qt.Equals, "GET")
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")

	}
}

func TestCacheKey(t *testing.T) {
	resetTest()
	c := qt.New(t)
	s.transport.CacheKey = func(req *http.Request) string {
		return "foo"
	}
	_, resp := doMethod(t, "GET", "/method", nil)
	c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
	_, ok := s.transport.Cache.Get("foo")
	c.Assert(ok, qt.Equals, true)
}

func TestEnableETagPair(t *testing.T) {
	resetTest()
	c := qt.New(t)
	s.transport.EnableETagPair = true

	{
		_, resp := doMethod(t, "GET", "/etag", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XETag1), qt.Equals, "124567")
		c.Assert(resp.Header.Get(XETag2), qt.Equals, "124567")
	}
	{
		_, resp := doMethod(t, "GET", "/etag", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XETag1), qt.Equals, "124567")
		c.Assert(resp.Header.Get(XETag2), qt.Equals, "124567")
	}

	// No HTTP caching in the following requests.
	{
		_, resp := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world1"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XETag1), qt.Equals, "48b21a691481958c34cc165011bdb9bc")
		c.Assert(resp.Header.Get(XETag2), qt.Equals, "48b21a691481958c34cc165011bdb9bc")
	}
	{
		_, resp := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world2"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XETag1), qt.Equals, "48b21a691481958c34cc165011bdb9bc")
		c.Assert(resp.Header.Get(XETag2), qt.Equals, "61b7d44bc024f189195b549bf094fbe8")

	}
}

func TestAlwaysUseCachedResponse(t *testing.T) {
	resetTest()
	c := qt.New(t)
	s.transport.AlwaysUseCachedResponse = func(req *http.Request, key string) bool {
		return req.Header.Get("Hello") == "world2"
	}

	{
		s, _ := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world1"})
		c.Assert(s, qt.Equals, "world1")
	}
	{
		s, _ := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world2"})
		c.Assert(s, qt.Equals, "world1")
	}
	{
		s, _ := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world3"})
		c.Assert(s, qt.Equals, "world3")
	}
}

func TestShouldCache(t *testing.T) {
	resetTest()
	c := qt.New(t)
	s.transport.AlwaysUseCachedResponse = func(req *http.Request, key string) bool {
		return true
	}
	s.transport.ShouldCache = func(req *http.Request, resp *http.Response, key string) bool {
		return req.Header.Get("Hello") == "world2"
	}
	{
		s, _ := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world1"})
		c.Assert(s, qt.Equals, "world1")
	}
	{
		s, _ := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world2"})
		c.Assert(s, qt.Equals, "world2")
	}
	{
		s, _ := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world3"})
		c.Assert(s, qt.Equals, "world2")
	}
}

func TestStaleCachedResponse(t *testing.T) {
	resetTest()
	s.transport.Cache = &staleCache{}
	s.transport.AlwaysUseCachedResponse = func(req *http.Request, key string) bool {
		return true
	}
	s.transport.EnableETagPair = true
	c := qt.New(t)
	{
		_, resp := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world1"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XETag1), qt.Equals, "48b21a691481958c34cc165011bdb9bc")
		c.Assert(resp.Header.Get(XETag2), qt.Equals, "48b21a691481958c34cc165011bdb9bc")
	}
	{
		_, resp := doMethod(t, "GET", "/helloheaderasbody", map[string]string{"Hello": "world2"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XETag1), qt.Equals, "48b21a691481958c34cc165011bdb9bc")
		c.Assert(resp.Header.Get(XETag2), qt.Equals, "61b7d44bc024f189195b549bf094fbe8")
	}
}

func TestAround(t *testing.T) {
	resetTest()
	c := qt.New(t)
	count := 0
	s.transport.Around = func(req *http.Request, key string) func() {
		count++
		return func() {
			count++
		}
	}
	_, resp := doMethod(t, "GET", "/method", nil)
	c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
	c.Assert(count, qt.Equals, 2)
}

func TestDontServeHeadResponseToGetRequest(t *testing.T) {
	resetTest()
	c := qt.New(t)
	doMethod(t, http.MethodHead, "/", nil)
	_, resp := doMethod(t, http.MethodGet, "/", nil)
	c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
}

func TestDontStorePartialRangeInCache(t *testing.T) {
	resetTest()
	c := qt.New(t)
	s.transport.MarkCachedResponses = true

	{
		body, resp := doMethod(t, "GET", "/range", map[string]string{"range": "bytes=4-9"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusPartialContent)
		c.Assert(body, qt.Equals, " text ")
		c.Assert(cacheSize(), qt.Equals, 0)
	}
	{
		body, resp := doMethod(t, "GET", "/range", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(body, qt.Equals, "Some text content")
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 1)
	}
	{
		body, resp := doMethod(t, "GET", "/range", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(body, qt.Equals, "Some text content")
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "1")
		c.Assert(cacheSize(), qt.Equals, 1)
	}
	{
		body, resp := doMethod(t, "GET", "/range", map[string]string{"range": "bytes=4-9"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusPartialContent)
		c.Assert(body, qt.Equals, " text ")
		c.Assert(cacheSize(), qt.Equals, 1)
	}
}

func TestCacheOnlyIfBodyRead(t *testing.T) {
	resetTest()
	{
		req, err := http.NewRequest("GET", s.server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
		// We do not read the body
		resp.Body.Close()
	}
	{
		req, err := http.NewRequest("GET", s.server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatalf("XFromCache header isn't blank")
		}
	}
}

func TestOnlyReadBodyOnDemand(t *testing.T) {
	resetTest()

	req, err := http.NewRequest("GET", s.server.URL+"/infinite", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.client.Do(req) // This shouldn't hang forever.
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10) // Only partially read the body.
	_, err = resp.Body.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestGetOnlyIfCachedHit(t *testing.T) {
	resetTest()
	c := qt.New(t)
	s.transport.MarkCachedResponses = true
	{
		_, resp := doMethod(t, "GET", "/", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 1)
	}
	{
		_, resp := doMethod(t, "GET", "/", map[string]string{"cache-control": "only-if-cached"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "1")
		c.Assert(cacheSize(), qt.Equals, 1)
	}
}

func TestGetOnlyIfCachedMiss(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	c := qt.New(t)
	_, resp := doMethod(t, "GET", "/", map[string]string{"cache-control": "only-if-cached"})
	c.Assert(resp.StatusCode, qt.Equals, http.StatusGatewayTimeout)
	c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
	c.Assert(cacheSize(), qt.Equals, 1)
}

func TestGetNoStoreRequest(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	c := qt.New(t)
	for i := 0; i < 2; i++ {

		_, resp := doMethod(t, "GET", "/", map[string]string{"cache-control": "no-store"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 0)

	}
}

func TestGetNoStoreResponse(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	c := qt.New(t)
	for i := 0; i < 2; i++ {
		_, resp := doMethod(t, "GET", "/nostore", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 0)
	}
}

func TestGetWithEtag(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	c := qt.New(t)

	{
		_, resp := doMethod(t, "GET", "/etag", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 1)
	}
	{
		_, resp := doMethod(t, "GET", "/etag", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "1")
		c.Assert(cacheSize(), qt.Equals, 1)
		_, ok := resp.Header["Connection"]
		c.Assert(ok, qt.IsFalse)
	}
}

func TestGetWithLastModified(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	c := qt.New(t)
	{
		_, resp := doMethod(t, "GET", "/lastmodified", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 1)
	}
	{
		_, resp := doMethod(t, "GET", "/lastmodified", nil)
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "1")
		c.Assert(cacheSize(), qt.Equals, 1)
	}
}

func TestGetWithVary(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	c := qt.New(t)
	{
		_, resp := doMethod(t, "GET", "/varyaccept", map[string]string{"Accept": "text/plain"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 1)
		c.Assert(resp.Header.Get("Vary"), qt.Equals, "Accept")
	}
	{
		_, resp := doMethod(t, "GET", "/varyaccept", map[string]string{"Accept": "text/plain"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "1")
	}
	{
		_, resp := doMethod(t, "GET", "/varyaccept", map[string]string{"Accept": "text/html"})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 1)
		c.Assert(resp.Header.Get("Vary"), qt.Equals, "Accept")
	}
	{
		_, resp := doMethod(t, "GET", "/varyaccept", map[string]string{"Accept": ""})
		c.Assert(resp.StatusCode, qt.Equals, http.StatusOK)
		c.Assert(resp.Header.Get(XFromCache), qt.Equals, "")
		c.Assert(cacheSize(), qt.Equals, 1)
		c.Assert(resp.Header.Get("Vary"), qt.Equals, "Accept")
	}
}

func TestGetWithDoubleVary(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	req, err := http.NewRequest("GET", s.server.URL+"/doublevary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("Accept-Language", "da, en-gb;q=0.8, en;q=0.7")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Vary") == "" {
			t.Fatalf(`Vary header is blank`)
		}
		_, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
	}
	req.Header.Set("Accept-Language", "")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
	req.Header.Set("Accept-Language", "da")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
}

func TestGetWith2VaryHeaders(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	// Tests that multiple Vary headers' comma-separated lists are
	// merged. See https://github.com/gregjones/httpcache/issues/27.
	const (
		accept         = "text/plain"
		acceptLanguage = "da, en-gb;q=0.8, en;q=0.7"
	)
	req, err := http.NewRequest("GET", s.server.URL+"/2varyheaders", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", acceptLanguage)
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Vary") == "" {
			t.Fatalf(`Vary header is blank`)
		}
		_, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
	}
	req.Header.Set("Accept-Language", "")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
	req.Header.Set("Accept-Language", "da")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
	req.Header.Set("Accept-Language", acceptLanguage)
	req.Header.Set("Accept", "")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
	req.Header.Set("Accept", "image/png")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
		_, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
	}
}

func TestGetVaryUnused(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	req, err := http.NewRequest("GET", s.server.URL+"/varyunused", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/plain")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Vary") == "" {
			t.Fatalf(`Vary header is blank`)
		}
		_, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
	}
}

func TestUpdateFields(t *testing.T) {
	resetTest()
	s.transport.MarkCachedResponses = true
	req, err := http.NewRequest("GET", s.server.URL+"/updatefields", nil)
	if err != nil {
		t.Fatal(err)
	}
	var counter, counter2 string
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		counter = resp.Header.Get("x-counter")
		_, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
		counter2 = resp.Header.Get("x-counter")
	}
	if counter == counter2 {
		t.Fatalf(`both "x-counter" values are equal: %v %v`, counter, counter2)
	}
}

// This tests the fix for https://github.com/gregjones/httpcache/issues/74.
// Previously, after validating a cached response, its StatusCode
// was incorrectly being replaced.
func TestCachedErrorsKeepStatus(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL+"/cachederror", nil)
	if err != nil {
		t.Fatal(err)
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("Status code isn't 404: %d", resp.StatusCode)
		}
	}
}

func TestParseCacheControl(t *testing.T) {
	resetTest()
	h := http.Header{}
	for range parseCacheControl(h) {
		t.Fatal("cacheControl should be empty")
	}

	h.Set("cache-control", "no-cache")
	{
		cc := parseCacheControl(h)
		if _, ok := cc["foo"]; ok {
			t.Error(`Value "foo" shouldn't exist`)
		}
		noCache, ok := cc["no-cache"]
		if !ok {
			t.Fatalf(`"no-cache" value isn't set`)
		}
		if noCache != "" {
			t.Fatalf(`"no-cache" value isn't blank: %v`, noCache)
		}
	}
	h.Set("cache-control", "no-cache, max-age=3600")
	{
		cc := parseCacheControl(h)
		noCache, ok := cc["no-cache"]
		if !ok {
			t.Fatalf(`"no-cache" value isn't set`)
		}
		if noCache != "" {
			t.Fatalf(`"no-cache" value isn't blank: %v`, noCache)
		}
		if cc["max-age"] != "3600" {
			t.Fatalf(`"max-age" value isn't "3600": %v`, cc["max-age"])
		}
	}
}

func TestNoCacheRequestExpiration(t *testing.T) {
	resetTest()
	respHeaders := http.Header{}
	respHeaders.Set("Cache-Control", "max-age=7200")

	reqHeaders := http.Header{}
	reqHeaders.Set("Cache-Control", "no-cache")
	if getFreshness(respHeaders, reqHeaders) != transparent {
		t.Fatal("freshness isn't transparent")
	}
}

func TestNoCacheResponseExpiration(t *testing.T) {
	resetTest()
	respHeaders := http.Header{}
	respHeaders.Set("Cache-Control", "no-cache")
	respHeaders.Set("Expires", "Wed, 19 Apr 3000 11:43:00 GMT")

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestReqMustRevalidate(t *testing.T) {
	resetTest()
	// not paying attention to request setting max-stale means never returning stale
	// responses, so always acting as if must-revalidate is set
	respHeaders := http.Header{}

	reqHeaders := http.Header{}
	reqHeaders.Set("Cache-Control", "must-revalidate")
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestRespMustRevalidate(t *testing.T) {
	resetTest()
	respHeaders := http.Header{}
	respHeaders.Set("Cache-Control", "must-revalidate")

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestFreshExpiration(t *testing.T) {
	resetTest()
	now := time.Now()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(time.RFC1123))
	respHeaders.Set("expires", now.Add(time.Duration(2)*time.Second).Format(time.RFC1123))

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 3 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestMaxAge(t *testing.T) {
	resetTest()
	now := time.Now()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(time.RFC1123))
	respHeaders.Set("cache-control", "max-age=2")

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 3 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestMaxAgeZero(t *testing.T) {
	resetTest()
	now := time.Now()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(time.RFC1123))
	respHeaders.Set("cache-control", "max-age=0")

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestBothMaxAge(t *testing.T) {
	resetTest()
	now := time.Now()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(time.RFC1123))
	respHeaders.Set("cache-control", "max-age=2")

	reqHeaders := http.Header{}
	reqHeaders.Set("cache-control", "max-age=0")
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestMinFreshWithExpires(t *testing.T) {
	resetTest()
	now := time.Now()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(time.RFC1123))
	respHeaders.Set("expires", now.Add(time.Duration(2)*time.Second).Format(time.RFC1123))

	reqHeaders := http.Header{}
	reqHeaders.Set("cache-control", "min-fresh=1")
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	reqHeaders = http.Header{}
	reqHeaders.Set("cache-control", "min-fresh=2")
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestEmptyMaxStale(t *testing.T) {
	resetTest()
	now := time.Now()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(time.RFC1123))
	respHeaders.Set("cache-control", "max-age=20")

	reqHeaders := http.Header{}
	reqHeaders.Set("cache-control", "max-stale")
	clock = &fakeClock{elapsed: 10 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 60 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}
}

func TestMaxStaleValue(t *testing.T) {
	resetTest()
	now := time.Now()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(time.RFC1123))
	respHeaders.Set("cache-control", "max-age=10")

	reqHeaders := http.Header{}
	reqHeaders.Set("cache-control", "max-stale=20")
	clock = &fakeClock{elapsed: 5 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 15 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 30 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func containsHeader(headers []string, header string) bool {
	for _, v := range headers {
		if http.CanonicalHeaderKey(v) == http.CanonicalHeaderKey(header) {
			return true
		}
	}
	return false
}

func TestGetEndToEndHeaders(t *testing.T) {
	resetTest()
	var (
		headers http.Header
		end2end []string
	)

	headers = http.Header{}
	headers.Set("content-type", "text/html")
	headers.Set("te", "deflate")

	end2end = getEndToEndHeaders(headers)
	if !containsHeader(end2end, "content-type") {
		t.Fatal(`doesn't contain "content-type" header`)
	}
	if containsHeader(end2end, "te") {
		t.Fatal(`doesn't contain "te" header`)
	}

	headers = http.Header{}
	headers.Set("connection", "content-type")
	headers.Set("content-type", "text/csv")
	headers.Set("te", "deflate")
	end2end = getEndToEndHeaders(headers)
	if containsHeader(end2end, "connection") {
		t.Fatal(`doesn't contain "connection" header`)
	}
	if containsHeader(end2end, "content-type") {
		t.Fatal(`doesn't contain "content-type" header`)
	}
	if containsHeader(end2end, "te") {
		t.Fatal(`doesn't contain "te" header`)
	}

	headers = http.Header{}
	end2end = getEndToEndHeaders(headers)
	if len(end2end) != 0 {
		t.Fatal(`non-zero end2end headers`)
	}

	headers = http.Header{}
	headers.Set("connection", "content-type")
	end2end = getEndToEndHeaders(headers)
	if len(end2end) != 0 {
		t.Fatal(`non-zero end2end headers`)
	}
}

type transportMock struct {
	response *http.Response
	err      error
}

func (t transportMock) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	return t.response, t.err
}

func TestStaleIfErrorRequest(t *testing.T) {
	resetTest()
	now := time.Now()
	tmock := transportMock{
		response: &http.Response{
			Status:     http.StatusText(http.StatusOK),
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Date":          []string{now.Format(time.RFC1123)},
				"Cache-Control": []string{"no-cache"},
			},
			Body: io.NopCloser(bytes.NewBuffer([]byte("some data"))),
		},
		err: nil,
	}
	tp := newMemoryCacheTransport()
	tp.Transport = &tmock

	// First time, response is cached on success
	r, _ := http.NewRequest("GET", "http://somewhere.com/", nil)
	r.Header.Set("Cache-Control", "stale-if-error")
	resp, err := tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// On failure, response is returned from the cache
	tmock.response = nil
	tmock.err = errors.New("some error")
	resp, err = tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
}

func TestStaleIfErrorRequestLifetime(t *testing.T) {
	resetTest()
	now := time.Now()
	tmock := transportMock{
		response: &http.Response{
			Status:     http.StatusText(http.StatusOK),
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Date":          []string{now.Format(time.RFC1123)},
				"Cache-Control": []string{"no-cache"},
			},
			Body: io.NopCloser(bytes.NewBuffer([]byte("some data"))),
		},
		err: nil,
	}
	tp := newMemoryCacheTransport()
	tp.Transport = &tmock

	// First time, response is cached on success
	r, _ := http.NewRequest("GET", "http://somewhere.com/", nil)
	r.Header.Set("Cache-Control", "stale-if-error=100")
	resp, err := tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// On failure, response is returned from the cache
	tmock.response = nil
	tmock.err = errors.New("some error")
	resp, err = tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}

	// Same for http errors
	tmock.response = &http.Response{StatusCode: http.StatusInternalServerError}
	tmock.err = nil
	resp, err = tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}

	// If failure last more than max stale, error is returned
	clock = &fakeClock{elapsed: 200 * time.Second}
	_, err = tp.RoundTrip(r)
	if err != tmock.err {
		t.Fatalf("got err %v, want %v", err, tmock.err)
	}
}

func TestStaleIfErrorResponse(t *testing.T) {
	resetTest()
	now := time.Now()
	tmock := transportMock{
		response: &http.Response{
			Status:     http.StatusText(http.StatusOK),
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Date":          []string{now.Format(time.RFC1123)},
				"Cache-Control": []string{"no-cache, stale-if-error"},
			},
			Body: io.NopCloser(bytes.NewBuffer([]byte("some data"))),
		},
		err: nil,
	}
	tp := newMemoryCacheTransport()
	tp.Transport = &tmock

	// First time, response is cached on success
	r, _ := http.NewRequest("GET", "http://somewhere.com/", nil)
	resp, err := tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// On failure, response is returned from the cache
	tmock.response = nil
	tmock.err = errors.New("some error")
	resp, err = tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
}

func TestStaleIfErrorResponseLifetime(t *testing.T) {
	resetTest()
	now := time.Now()
	tmock := transportMock{
		response: &http.Response{
			Status:     http.StatusText(http.StatusOK),
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Date":          []string{now.Format(time.RFC1123)},
				"Cache-Control": []string{"no-cache, stale-if-error=100"},
			},
			Body: io.NopCloser(bytes.NewBuffer([]byte("some data"))),
		},
		err: nil,
	}
	tp := newMemoryCacheTransport()
	tp.Transport = &tmock

	// First time, response is cached on success
	r, _ := http.NewRequest("GET", "http://somewhere.com/", nil)
	resp, err := tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// On failure, response is returned from the cache
	tmock.response = nil
	tmock.err = errors.New("some error")
	resp, err = tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}

	// If failure last more than max stale, error is returned
	clock = &fakeClock{elapsed: 200 * time.Second}
	_, err = tp.RoundTrip(r)
	if err != tmock.err {
		t.Fatalf("got err %v, want %v", err, tmock.err)
	}
}

// This tests the fix for https://github.com/gregjones/httpcache/issues/74.
// Previously, after a stale response was used after encountering an error,
// its StatusCode was being incorrectly replaced.
func TestStaleIfErrorKeepsStatus(t *testing.T) {
	resetTest()
	now := time.Now()
	tmock := transportMock{
		response: &http.Response{
			Status:     http.StatusText(http.StatusNotFound),
			StatusCode: http.StatusNotFound,
			Header: http.Header{
				"Date":          []string{now.Format(time.RFC1123)},
				"Cache-Control": []string{"no-cache"},
			},
			Body: io.NopCloser(bytes.NewBuffer([]byte("some data"))),
		},
		err: nil,
	}
	tp := newMemoryCacheTransport()
	tp.Transport = &tmock

	// First time, response is cached on success
	r, _ := http.NewRequest("GET", "http://somewhere.com/", nil)
	r.Header.Set("Cache-Control", "stale-if-error")
	resp, err := tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// On failure, response is returned from the cache
	tmock.response = nil
	tmock.err = errors.New("some error")
	resp, err = tp.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("Status wasn't 404: %d", resp.StatusCode)
	}
}

// Test that http.Client.Timeout is respected when cache transport is used.
// That is so as long as request cancellation is propagated correctly.
// In the past, that required CancelRequest to be implemented correctly,
// but modern http.Client uses Request.Cancel (or request context) instead,
// so we don't have to do anything.
func TestClientTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout test in short mode") // Because it takes at least 3 seconds to run.
	}
	resetTest()
	client := &http.Client{
		Transport: newMemoryCacheTransport(),
		Timeout:   time.Second,
	}
	started := time.Now()
	resp, err := client.Get(s.server.URL + "/3seconds")
	taken := time.Since(started)
	if err == nil {
		t.Error("got nil error, want timeout error")
	}
	if resp != nil {
		t.Error("got non-nil resp, want nil resp")
	}
	if taken >= 2*time.Second {
		t.Error("client.Do took 2+ seconds, want < 2 seconds")
	}
}

func doMethod(t testing.TB, method string, p string, headers map[string]string) (string, *http.Response) {
	t.Helper()
	req, err := http.NewRequest(method, s.server.URL+p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) > 0 {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, err = io.Copy(&buf, resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	err = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	return buf.String(), resp
}

// newMemoryCacheTransport returns a new Transport using the in-memory cache implementation
func newMemoryCacheTransport() *Transport {
	c := newMemoryCache()
	t := &Transport{Cache: c}
	return t
}

// memoryCache is an implemtation of Cache that stores responses in an in-memory map.
type memoryCache struct {
	mu    sync.RWMutex
	items map[string][]byte
}

// newMemoryCache returns a new Cache that will store items in an in-memory map
func newMemoryCache() *memoryCache {
	c := &memoryCache{items: map[string][]byte{}}
	return c
}

func (c *memoryCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Get returns the []byte representation of the response and true if present, false if not
func (c *memoryCache) Get(key string) (resp []byte, ok bool) {
	c.mu.RLock()
	resp, ok = c.items[key]
	c.mu.RUnlock()
	return resp, ok
}

// Set saves response resp to the cache with key
func (c *memoryCache) Set(key string, resp []byte) {
	c.mu.Lock()
	c.items[key] = resp
	c.mu.Unlock()
}

// Delete removes key from the cache
func (c *memoryCache) Delete(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

var _ Cache = &staleCache{}

type staleCache struct {
	val []byte
}

func (c *staleCache) Get(key string) ([]byte, bool) {
	return c.val, false
}

func (c *staleCache) Set(key string, resp []byte) {
	c.val = resp
}

func (c *staleCache) Delete(key string) {
	c.val = nil
}

func (c *staleCache) Size() int {
	return 1
}
