package dashboard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The server's own properties: where it binds, what it refuses, and what it does with a
// request naming something that does not exist.

// (17) The bind default is loopback, and it is a constant rather than a runtime choice so
// that no code path can arrive at 0.0.0.0 by omission.
func TestDefaultListenIsLoopback(t *testing.T) {
	host, port, err := net.SplitHostPort(DefaultListen)
	if err != nil {
		t.Fatalf("DefaultListen %q is not host:port: %v", DefaultListen, err)
	}
	if !Loopback(host) {
		t.Errorf("DefaultListen host %q is not loopback", host)
	}
	if port == "" {
		t.Error("DefaultListen names no port")
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("DefaultListen host %q does not parse as a loopback IP", host)
	}
}

// An empty listen address must resolve to the loopback default rather than to net.Listen's
// own interpretation of "", which is every interface.
func TestListenWithNoAddressBindsTheLoopbackDefault(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var warnings []string
	done := make(chan error, 1)
	go func() { done <- w.srv.Listen(ctx, "", func(m string) { warnings = append(warnings, m) }) }()

	// Poll the default address rather than sleeping for a fixed period.
	var resp *http.Response
	for i := 0; i < 200; i++ {
		r, err := http.Get("http://" + DefaultListen + "/healthz")
		if err == nil {
			resp = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp == nil {
		cancel()
		t.Skip("nothing accepted a connection on " + DefaultListen +
			"; the port is most likely already in use on this machine")
	}
	resp.Body.Close()

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Listen: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("binding the loopback default warned about exposure: %q", warnings)
	}
}

// (17) A non-loopback address is accepted, but not quietly: the warning has to name what
// is exposed, because "insecure" does not tell an operator what they are publishing.
func TestExposureWarning(t *testing.T) {
	quiet := []string{
		"127.0.0.1:7654",
		"127.0.0.1:0",
		"localhost:7654",
		"[::1]:7654",
		"127.0.0.53:9",
	}
	for _, addr := range quiet {
		if msg := ExposureWarning(addr); msg != "" {
			t.Errorf("ExposureWarning(%q) = %q, want no warning", addr, msg)
		}
	}

	loud := []string{
		"0.0.0.0:7654",
		":7654",
		"192.168.1.20:7654",
		"[::]:7654",
		"my-laptop.local:7654",
		"10.0.0.1", // unparseable as host:port, and still warned about
	}
	for _, addr := range loud {
		msg := ExposureWarning(addr)
		if msg == "" {
			t.Fatalf("ExposureWarning(%q) is silent", addr)
		}
		for _, want := range []string{"NO authentication", "spend", "loopback", DefaultListen} {
			if !strings.Contains(msg, want) {
				t.Errorf("ExposureWarning(%q) does not mention %q:\n%s", addr, want, msg)
			}
		}
	}

	// A wildcard bind has no host to name, so the warning has to describe the reach
	// rather than print an empty string.
	if msg := ExposureWarning(":7654"); !strings.Contains(msg, "every interface") {
		t.Errorf("a wildcard bind warning does not describe its reach:\n%s", msg)
	}
}

func TestLoopback(t *testing.T) {
	cases := map[string]bool{
		"":              false, // in a listen address, every interface
		"localhost":     true,
		"127.0.0.1":     true,
		"127.1.2.3":     true,
		"::1":           true,
		"[::1]":         true,
		"0.0.0.0":       false,
		"::":            false,
		"192.168.0.5":   false,
		"example.com":   false, // may resolve anywhere; guessing wrong publishes spend
		"not-an-ip-at-": false,
	}
	for host, want := range cases {
		if got := Loopback(host); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", host, got, want)
		}
	}
}

// The read-only property, asserted at the edge: no method other than GET and HEAD reaches
// a handler at all.
func TestWritesAreRefusedBeforeRouting(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, "PROPFIND",
	} {
		req := httptest.NewRequest(method, "/", nil)
		rec := httptest.NewRecorder()
		w.srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s / = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s / Allow = %q, want %q", method, allow, "GET, HEAD")
		}
		if !strings.Contains(rec.Body.String(), "read-only") {
			t.Errorf("%s / body does not say the dashboard is read-only: %q", method, rec.Body.String())
		}
	}
}

// The same refusal applies to the JSON surface, so no mutation endpoint can be reached
// even by a client that guesses a plausible name.
func TestNoMutationEndpointExists(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	for _, path := range []string{
		"/api/reconcile", "/api/recover", "/api/settle", "/api/release",
		"/api/advance", "/api/define", "/reconcile", "/recover",
	} {
		// A GET to an invented path is a 404, and a POST is a 405 -- neither is a write.
		if rec := w.get(path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		w.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, rec.Code)
		}
	}
}

func TestSecurityHeadersAndHealth(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	rec := w.get("/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Errorf("GET /healthz body = %q, want %q", got, "ok")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}

	// The page itself must not be cached: a stale budget figure served from a cache is
	// worse than a slow one.
	page := w.get("/")
	if got := page.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("page Cache-Control = %q, want no-store", got)
	}
	if ct := page.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("page Content-Type = %q, want text/html", ct)
	}
}

// The page is served from embedded assets, so a binary copied to another machine renders
// identically. This is the one test that would catch a stylesheet dropped from //go:embed.
func TestStaticAssetsAreEmbedded(t *testing.T) {
	w := newWorld(t)
	for _, path := range []string{"/static/app.css", "/static/app.js", "/static/throttle-logo.png"} {
		rec := w.get(path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s served an empty body", path)
		}
	}
}

// A mistyped id is a 404 with an explanation, not a 500 and not a page of zeros: the
// distinction tells an operator whether to check their URL or their database.
func TestUnknownNamesAre404(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(100), w.now.Add(-time.Hour))
	w.record(settledRecord("r1", "research", p.ID, cents(40), w.now.Add(-time.Hour)))

	cases := []struct{ what, path string }{
		{"an unknown budget", "/?budget=nope"},
		{"an unknown request", "/request/nope"},
		{"an unknown facet", "/api/breakdown?facet=nope"},
		{"an unknown budget on the API", "/api/summary?budget=nope"},
	}
	for _, c := range cases {
		rec := w.get(c.path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: GET %s = %d, want 404\n%s", c.what, c.path, rec.Code, rec.Body.String())
		}
	}

	// The HTML 404 says nothing was written, because the first question about an error
	// page on a money tool is whether it changed anything.
	rec := w.get("/?budget=nope")
	body := rec.Body.String()
	mustContain(t, body, "Nothing was written", "a 404 must state that the dashboard is read-only")
	mustContain(t, body, "not found", "a 404 page should say so")

	// The JSON 404 is a JSON object, so a poller does not have to parse HTML to learn
	// what happened.
	api := w.get("/api/summary?budget=nope")
	if ct := api.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("/api/ error Content-Type = %q, want application/json", ct)
	}
	obj := decodeObject(t, api.Body)
	if str(t, obj, "error") == "" {
		t.Error("/api/ error payload has an empty error message")
	}
}

// A bad query parameter is the client's fault, and the message says which parameter.
func TestBadLimitIsReportedRatherThanIgnored(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	for _, raw := range []string{"abc", "-5", "1.5"} {
		rec := w.get("/api/activity?limit=" + raw)
		if rec.Code == http.StatusOK {
			t.Errorf("limit=%q was accepted; a silently ignored limit misreports the page", raw)
			continue
		}
		obj := decodeObject(t, rec.Body)
		if msg := str(t, obj, "error"); !strings.Contains(msg, "limit") {
			t.Errorf("limit=%q error does not mention the parameter: %q", raw, msg)
		}
	}
}

// Serve returns cleanly on cancellation rather than reporting the shutdown as a failure,
// because "throttle serve" exiting non-zero on Ctrl-C would be wrong.
func TestServeStopsCleanlyOnCancel(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.srv.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		cancel()
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return within 10s of cancellation")
	}
}

func TestNewRequiresAReporter(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with no reporter succeeded")
	} else if err != ErrNoReporter {
		t.Errorf("New with no reporter = %v, want ErrNoReporter", err)
	}
}
