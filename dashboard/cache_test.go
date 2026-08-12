package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/report"
)

// Everything on this dashboard is a snapshot of current budget state, and a cached
// snapshot is a stale spend figure presented as a current one. A browser that serves a
// five-minute-old page from its cache shows a budget that has moved since, with no
// indication that it has -- and the "as of" timestamp in the corner would be cached along
// with it, so the page would lie about its own freshness.
//
// The default lives in ServeHTTP rather than in each handler, so this test walks every
// route the mux knows about rather than the handful a page render happens to touch.

// Dynamic responses carry Cache-Control: no-store; embedded assets do not.
func TestDynamicResponsesAreNeverCached(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", dollars(40), at)
	w.record(settledRecord("req1", "research", p.ID, dollars(40), at))
	w.hold("h1", "research", dollars(5))

	// Every dynamic route, including the ones a page render does not exercise: an
	// endpoint added later is uncacheable by default, and this list is what proves the
	// default rather than a habit is doing the work.
	dynamic := []string{
		"/",
		"/?budget=research",
		"/?budget=research&period=" + p.ID,
		"/request/req1",
		"/api/summary",
		"/api/summary?budget=research",
		"/api/activity?budget=research",
		"/api/activity?budget=research&unresolved=1",
		"/api/breakdown?budget=research",
		"/api/breakdown?budget=research&facet=" + string(report.FacetModel),
		"/api/timeline?budget=research",
		"/healthz",
	}
	for _, path := range dynamic {
		rec := w.get(path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200\n%s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, got)
		}
	}

	// The embedded assets are the exception, and are allowed to be cached because a build
	// serves exactly one version of each and none of them says anything about a budget.
	for _, path := range []string{"/static/app.css", "/static/app.js", "/static/throttle-logo.png"} {
		rec := w.get(path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		got := rec.Header().Get("Cache-Control")
		if strings.Contains(got, "no-store") {
			t.Errorf("GET %s Cache-Control = %q; an immutable embedded asset need not be "+
				"refetched on every page load", path, got)
		}
		if !strings.Contains(got, "max-age=") {
			t.Errorf("GET %s Cache-Control = %q, want a max-age", path, got)
		}
	}
}

// An error response is a snapshot too. A cached 404 for a budget that has since been
// defined, or a cached 500 from a database that has since recovered, would outlive the
// condition that produced it -- and the reader would have no way to tell the page apart
// from a current one.
func TestErrorResponsesAreNeverCachedEither(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	for _, path := range []string{
		"/?budget=nonesuch",                        // HTML 404
		"/request/nonesuch",                        // detail 404
		"/api/summary?budget=nonesuch",             // JSON 404
		"/api/breakdown?budget=research&facet=xyz", // unknown facet
		"/api/activity?budget=research&limit=-4",   // bad parameter
	} {
		rec := w.get(path)
		if rec.Code == http.StatusOK {
			t.Fatalf("GET %s = 200, want an error status", path)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s (%d) Cache-Control = %q, want no-store", path, rec.Code, got)
		}
	}

	// And the refusal a write receives, which does not reach the mux at all.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("POST / Cache-Control = %q, want no-store", got)
	}
}
