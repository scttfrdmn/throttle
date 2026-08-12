// Package dashboard is throttle's local read-only web UI.
//
// It is a view over the read model in throttle/report, which is itself a view over the
// ledger and the activity store. That layering is deliberate and one-directional:
//
//	ledger + activity  ->  report (money, time, counts)  ->  dashboard (pixels, strings)
//
// No accounting happens here. This package formats figures, computes pixel coordinates,
// and serves HTML and JSON; it never adds two amounts to produce a third, never decides
// what a cost means, and never writes anything. There is no handler in this file that
// mutates state, and the interfaces it depends on offer no verb that could.
//
// # Local by default
//
// The server binds loopback unless told otherwise, and there is no authentication. A
// dashboard that exposes what a team spends on which models is worth protecting, so a
// non-loopback listen address is accepted only with a warning that says exactly what is
// being exposed. Authentication belongs with a multi-user deployment, not here.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scttfrdmn/throttle/report"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// DefaultListen is the default bind address: loopback, and a port unlikely to collide.
//
// Loopback is not a suggestion. Binding 0.0.0.0 by default would publish a spend and
// model-usage history to every host that can reach the machine, on the strength of
// somebody having run one command.
const DefaultListen = "127.0.0.1:7654"

// Reporter is the read model this package renders.
//
// A consumer-defined interface over *report.Reporter, listing exactly the queries the UI
// issues. It is what makes the read-only property structural rather than a convention:
// there is no method here that could settle, release, reconcile, or advance anything,
// so no amount of refactoring inside a handler can turn a page view into a write.
type Reporter interface {
	Now() time.Time
	HasActivity() bool

	Summary(ctx context.Context, budgetID string) (report.Summary, error)
	PositionIn(ctx context.Context, budgetID, periodID string) (report.Position, error)
	Tree(ctx context.Context) (report.Tree, error)
	Periods(ctx context.Context, budgetID string) ([]report.PeriodOption, error)
	Timeline(ctx context.Context, budgetID, periodID string) (report.Timeline, error)
	Activity(ctx context.Context, q report.ActivityQuery) (report.ActivityPage, error)
	Unresolved(ctx context.Context, q report.ActivityQuery) (report.ActivityPage, error)
	Breakdowns(ctx context.Context, facets []report.Facet, q report.ActivityQuery) ([]report.Breakdown, error)
	Reservations(ctx context.Context, budgetID string, limit int) ([]report.Hold, error)
	Bookkeeping(ctx context.Context, budgetID, periodID string) report.Bookkeeping
	Detail(ctx context.Context, requestID string) (report.Detail, error)
}

// Config builds a Server.
type Config struct {
	// Reporter is required.
	Reporter Reporter

	// Version is shown in the footer, so a screenshot identifies the build it came
	// from.
	Version string

	// ActivityLimit caps the recent-activity table. Zero uses a sensible default.
	ActivityLimit int
}

// Server serves the dashboard.
type Server struct {
	rep   Reporter
	tpl   *template.Template
	mux   *http.ServeMux
	ver   string
	limit int
}

// ErrNoReporter reports a Server built without a read model.
var ErrNoReporter = errors.New("dashboard: a reporter is required")

// DefaultActivityLimit is how many requests the table shows before saying it truncated.
//
// Exported alongside DefaultListen so that config, which declares its own copies to avoid
// depending on this package, can assert the two agree. A copied constant with no test is a
// constant that drifts.
const DefaultActivityLimit = 100

// New builds a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Reporter == nil {
		return nil, ErrNoReporter
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse templates: %w", err)
	}
	limit := cfg.ActivityLimit
	if limit <= 0 {
		limit = DefaultActivityLimit
	}
	s := &Server{rep: cfg.Reporter, tpl: tpl, ver: cfg.Version, limit: limit}
	s.routes()
	return s, nil
}

// funcs are the template helpers. There is no money formatting here: templates receive
// finished strings, so a template cannot invent a currency figure.
var funcs = template.FuncMap{
	"lower": strings.ToLower,
}

func (s *Server) routes() {
	s.mux = http.NewServeMux()

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Unreachable: the directory is embedded at build time.
		panic(err)
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheFor(24*time.Hour, http.FileServer(http.FS(static)))))

	s.mux.HandleFunc("GET /{$}", s.handlePage)
	s.mux.HandleFunc("GET /request/{id}", s.handleDetail)

	// The JSON surface is small, internal, and read-only. It exists so a poll can
	// refresh figures without reloading the page, not as a public API: it is
	// unversioned in the URL precisely because nothing outside this binary should
	// depend on it yet. That is settled for v0.1 rather than deferred -- these are
	// internal dashboard interfaces, are not documented as a supported integration
	// point, and versioning can be designed if and when throttle deliberately exposes
	// a public API.
	s.mux.HandleFunc("GET /api/summary", s.apiSummary)
	s.mux.HandleFunc("GET /api/activity", s.apiActivity)
	s.mux.HandleFunc("GET /api/breakdown", s.apiBreakdown)
	s.mux.HandleFunc("GET /api/timeline", s.apiTimeline)

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
}

// ServeHTTP dispatches, and refuses anything that is not a read.
//
// The method check is deliberately in front of the mux rather than left to the route
// patterns: this slice adds no write surface, and a 405 on POST is a clearer statement of
// that than a 404 from an unregistered pattern.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The page embeds no third-party anything, so it can afford to say so. This is
	// hygiene rather than a security boundary: the boundary is the loopback bind.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Every response is uncacheable until a handler says otherwise, and the only handler
	// that says otherwise serves immutable embedded assets.
	//
	// The default is set here, in front of the mux and in front of the method check,
	// rather than in each handler. A page and a JSON payload are both snapshots of current
	// budget state: a cached one is a stale spend figure presented as a current one, and
	// no reader can tell -- the "as of" timestamp in the corner would be cached along with
	// it, so the page would lie about its own freshness. An error is a snapshot too: a
	// cached 404 for a budget that has since been defined outlives the condition that
	// produced it. Setting the header per-handler would mean a new endpoint is uncacheable
	// only if whoever added it remembered, and the mistake is invisible in a browser
	// because the first render looks perfect. Defaulting to no-store makes forgetting safe
	// and caching deliberate.
	w.Header().Set("Cache-Control", "no-store")

	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the dashboard is read-only", http.StatusMethodNotAllowed)
		return
	}

	s.mux.ServeHTTP(w, r)
}

// cacheFor overrides the no-store default for content that cannot go stale.
//
// The embedded assets ship inside the binary, so a given build serves exactly one version
// of each. Nothing about a budget is in them.
func cacheFor(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
}

// Listen starts the server on addr and blocks until ctx is cancelled.
//
// warn is called with a message when the address is not loopback, so the caller decides
// how to present it -- a CLI writes it to stderr; a test asserts on it.
func (s *Server) Listen(ctx context.Context, addr string, warn func(string)) error {
	if addr == "" {
		addr = DefaultListen
	}
	if msg := ExposureWarning(addr); msg != "" && warn != nil {
		warn(msg)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves on an existing listener until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		// A dashboard read is a handful of SQLite queries. A minute is generous and
		// still bounds a wedged handler.
		WriteTimeout: time.Minute,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// ExposureWarning is the warning for a listen address that is not loopback, empty when
// the address is safe.
//
// It names what is exposed rather than saying "insecure": an operator deciding whether
// to bind an interface needs to know it is publishing budget names, model usage, and
// per-request costs with no login in front of them.
func ExposureWarning(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// An unparseable address is not a reason to stay quiet: net.Listen will either
		// reject it or bind something, and if it binds, the operator should have been
		// told.
		host = addr
	}
	if Loopback(host) {
		return ""
	}
	shown := host
	if shown == "" {
		shown = "every interface on this host"
	}
	return fmt.Sprintf(
		"warning: the dashboard is listening on %s, not loopback.\n"+
			"  It has NO authentication. Anyone who can reach %s can read your budget\n"+
			"  names, allocations, spend, model and provider usage, and per-request costs.\n"+
			"  Bind %s instead unless you have put your own access control in front of it.",
		addr, shown, DefaultListen)
}

// Loopback reports whether a host from a listen address is loopback-only.
//
// An empty host is not: in a listen address it means every interface, which is the case
// the warning exists for.
func Loopback(host string) bool {
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	// A bracketed IPv6 literal, if SplitHostPort was not what produced this.
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname that is not "localhost" may resolve anywhere. Treated as exposed,
		// because the failure mode of guessing wrong is publishing spend data.
		return false
	}
	return ip.IsLoopback()
}

// handlePage renders the dashboard.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	page, _, err := s.buildPage(r.Context(), r.URL.Query().Get("budget"), r.URL.Query().Get("period"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "page.html", page)
}

// handleDetail renders one request.
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	d, err := s.rep.Detail(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	detail := buildDetail(d, s.rep.Now())
	detail.Version = s.ver
	s.render(w, r, "detail.html", detail)
}

// buildPage assembles everything one dashboard render needs.
//
// The order matters. The budget list comes first, because an empty one is the first-run
// page rather than an error; then the selected budget's position, which is money and
// comes from the ledger; then request history, whose failure degrades the page instead of
// replacing it.
//
// It returns the summary it resolved alongside the page, so the JSON endpoint reports the
// same period the HTML does rather than resolving the selection a second time and risking
// the two describing different envelopes.
func (s *Server) buildPage(ctx context.Context, budgetID, periodID string) (Page, report.Summary, error) {
	at := s.rep.Now()
	page := Page{
		Title:       "throttle",
		At:          at,
		AtDisplay:   timestamp(at),
		Version:     s.ver,
		HasActivity: s.rep.HasActivity(),
	}

	tree, err := s.rep.Tree(ctx)
	if err != nil {
		return Page{}, report.Summary{}, err
	}
	if tree.Empty() {
		// No budgets defined at all. A legitimate state with its own page, not a 404 and
		// not a wall of zeros.
		page.Empty = true
		page.Title = "throttle — no budgets"
		return page, report.Summary{}, nil
	}

	flat := tree.Flatten()
	if budgetID == "" {
		budgetID = flat[0].Position.BudgetID
	}
	if !containsBudget(flat, budgetID) {
		return Page{}, report.Summary{}, fmt.Errorf("%w: no budget %q", errNotFound, budgetID)
	}
	page.Budget = budgetID
	page.Budgets = buildBudgetOptions(tree, budgetID)
	page.Hierarchy = buildTree(tree, budgetID)

	sum, err := s.rep.Summary(ctx, budgetID)
	if err != nil {
		return Page{}, report.Summary{}, err
	}
	page.Title = "throttle — " + budgetID
	current := sum.Position.Period.ID

	// A prior period selected explicitly is read in its own right, with the clock
	// clamped into it, rather than by pretending the current period's figures apply.
	pos := sum.Position
	if periodID != "" && periodID != pos.Period.ID {
		p, err := s.rep.PositionIn(ctx, budgetID, periodID)
		if err != nil {
			return Page{}, report.Summary{}, err
		}
		pos = p
		book := s.rep.Bookkeeping(ctx, budgetID, periodID)
		sum.Position = pos
		sum.Health, sum.Activity = book.Health, book.Activity
		sum.ActivityAvailable, sum.ActivityError = book.Available, book.Error
		sum.Health.ExpiredHolds = pos.ExpiredHolds
		sum.Health.ExpiredAmount = pos.ExpiredAmount
	}
	page.PeriodID = pos.Period.ID
	page.Historic = pos.Period.ID != current

	if opts, err := s.rep.Periods(ctx, budgetID); err == nil {
		page.Periods = buildPeriodOptions(opts, pos.Period.ID)
	}

	borrow := pos.Period.Envelope.Borrow
	page.Position = buildPosition(pos, sum.Binding, borrow)
	page.Pressure = gaugeView(pos.Pressure)
	page.Bank = bankView(pos)
	page.Rates = buildRates(pos)
	page.EndState = buildEndState(pos)
	page.Rollover = buildRollover(pos)

	tl, err := s.rep.Timeline(ctx, budgetID, pos.Period.ID)
	if err != nil {
		return Page{}, report.Summary{}, err
	}
	page.Chart = chartView(tl, pos)

	if holds, err := s.rep.Reservations(ctx, budgetID, 0); err == nil {
		page.Holds = buildHolds(holds)
	}

	// Request history from here down. Every failure below is reported as a degraded
	// panel: money already came from the ledger, and a broken telemetry database must
	// not blank a page that can still answer "where does this budget stand?".
	q := report.ActivityQuery{BudgetID: budgetID, PeriodID: pos.Period.ID, Limit: s.limit}

	acts, actErr := s.rep.Activity(ctx, q)
	available, errText := sum.ActivityAvailable, sum.ActivityError
	if actErr != nil {
		available, errText = false, actErr.Error()
	}
	page.Activity = buildActivity(acts, available, errText)

	if bs, err := s.rep.Breakdowns(ctx, report.Facets, report.ActivityQuery{
		BudgetID: budgetID, PeriodID: pos.Period.ID,
	}); err == nil {
		page.Breakdowns = buildBreakdowns(bs)
	}

	unresolved, _ := s.rep.Unresolved(ctx, report.ActivityQuery{
		BudgetID: budgetID, PeriodID: pos.Period.ID, Limit: 50,
	})
	page.Recon = buildRecon(sum.Health, unresolved, available, errText)

	page.Notices = notices(sum, tl, at)
	return page, sum, nil
}

func containsBudget(flat []report.FlatNode, id string) bool {
	for _, fn := range flat {
		if fn.Position.BudgetID == id {
			return true
		}
	}
	return false
}

// errNotFound is the sentinel for a request naming something that does not exist.
var errNotFound = errors.New("not found")

// render executes a template into a buffer first.
//
// Buffering matters: a template error halfway through a page would otherwise leave a
// half-written 200 response, which reads as a dashboard showing a truncated budget
// rather than as a fault.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, r, fmt.Errorf("render %s: %w", name, err))
		return
	}
	// Cache-Control comes from ServeHTTP, which sets no-store for everything the mux
	// routes. Restating it here would be a second place for the policy to live.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, buf.String())
}

// fail maps an error to a status and a page.
//
// A mistyped budget id is a 404, not a 500: the distinction is what tells an operator
// whether to check their URL or their database.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	if report.NotFound(err) || errors.Is(err, errNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, report.ErrNoActivity) {
		// Request history is optional. Asking for it when it is not configured is a
		// misconfiguration, not a server fault.
		status = http.StatusNotFound
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSONError(w, status, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	page := ErrorPage{Status: status, Message: err.Error(), Version: s.ver}
	if status == http.StatusNotFound {
		page.Title = "throttle — not found"
	} else {
		page.Title = "throttle — error"
	}
	if err := s.tpl.ExecuteTemplate(w, "error.html", page); err != nil {
		fmt.Fprintf(w, "<pre>%s</pre>", template.HTMLEscapeString(page.Message))
	}
}

// ErrorPage is the error template's data.
type ErrorPage struct {
	Title   string
	Status  int
	Message string
	Version string
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// writeJSON writes a payload. Cache-Control: no-store comes from ServeHTTP, so a figure
// a poll refreshes cannot be served from a cache.
func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		// The status is already written by now; there is nothing honest left to do but
		// stop.
		return
	}
}

// apiSummary is the poll endpoint behind the gauge and the figures.
func (s *Server) apiSummary(w http.ResponseWriter, r *http.Request) {
	budgetID := r.URL.Query().Get("budget")
	periodID := r.URL.Query().Get("period")

	page, sum, err := s.buildPage(r.Context(), budgetID, periodID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if page.Empty {
		// No budgets is a state, not an error, and a poller needs to be able to tell the
		// difference without reading a status code.
		s.writeJSON(w, map[string]any{"empty": true})
		return
	}
	s.writeJSON(w, buildSummaryJSON(sum, page))
}

func (s *Server) apiActivity(w http.ResponseWriter, r *http.Request) {
	q, err := s.query(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if r.URL.Query().Get("unresolved") == "1" {
		page, err := s.rep.Unresolved(r.Context(), q)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.writeJSON(w, activityFrom(buildActivity(page, true, "")))
		return
	}
	page, err := s.rep.Activity(r.Context(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, activityFrom(buildActivity(page, true, "")))
}

func activityFrom(v ActivityView) activityJSON {
	return activityJSON{
		Events:       v.Events,
		Truncated:    v.Truncated,
		Limit:        v.Limit,
		PageSpend:    v.PageSpend,
		PageComplete: v.PageComplete,
		Requests:     v.Requests,
		Available:    v.Available,
		Error:        v.Error,
	}
}

func (s *Server) apiBreakdown(w http.ResponseWriter, r *http.Request) {
	q, err := s.query(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A breakdown aggregates the period, so it is not limited to a page of rows.
	q.Limit = 0

	facets := report.Facets
	if raw := r.URL.Query().Get("facet"); raw != "" {
		f := report.Facet(raw)
		if !knownFacet(f) {
			s.fail(w, r, fmt.Errorf("%w: no facet %q", errNotFound, raw))
			return
		}
		facets = []report.Facet{f}
	}

	bs, err := s.rep.Breakdowns(r.Context(), facets, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, breakdownJSON{Breakdowns: buildBreakdowns(bs)})
}

func knownFacet(f report.Facet) bool {
	for _, known := range report.Facets {
		if known == f {
			return true
		}
	}
	return false
}

func (s *Server) apiTimeline(w http.ResponseWriter, r *http.Request) {
	budgetID, periodID, err := s.scope(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tl, err := s.rep.Timeline(r.Context(), budgetID, periodID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pos, err := s.rep.PositionIn(r.Context(), budgetID, tl.PeriodID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, buildTimelineJSON(tl, chartView(tl, pos)))
}

// query builds an activity query from the request, resolving the budget the same way the
// page does so an endpoint and the page it refreshes never describe different budgets.
func (s *Server) query(r *http.Request) (report.ActivityQuery, error) {
	budgetID, periodID, err := s.scope(r)
	if err != nil {
		return report.ActivityQuery{}, err
	}
	q := report.ActivityQuery{BudgetID: budgetID, PeriodID: periodID, Limit: s.limit}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return report.ActivityQuery{}, fmt.Errorf("limit %q is not a number of rows", raw)
		}
		q.Limit = n
	}
	return q, nil
}

// scope resolves the budget and period a request is about.
func (s *Server) scope(r *http.Request) (string, string, error) {
	ctx := r.Context()
	budgetID := r.URL.Query().Get("budget")
	periodID := r.URL.Query().Get("period")

	if budgetID == "" {
		tree, err := s.rep.Tree(ctx)
		if err != nil {
			return "", "", err
		}
		flat := tree.Flatten()
		if len(flat) == 0 {
			return "", "", fmt.Errorf("%w: no budgets are defined", errNotFound)
		}
		budgetID = flat[0].Position.BudgetID
	}
	if periodID == "" {
		sum, err := s.rep.Summary(ctx, budgetID)
		if err != nil {
			return "", "", err
		}
		periodID = sum.Position.Period.ID
	}
	return budgetID, periodID, nil
}
