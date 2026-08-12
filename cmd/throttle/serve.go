package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	activitysqlite "throttle/activity/sqlite"
	"throttle/dashboard"
	"throttle/report"
)

// serveCmd runs the local dashboard.
//
// It is a read-only view over the two stores the rest of the CLI writes to. Nothing here
// reserves, settles, releases, recovers, or advances anything: reconciliation stays an
// explicit "throttle reconcile", because a page load is not a decision to move money.
//
// The listener binds loopback unless told otherwise, and says so loudly when it is not,
// because the dashboard has no authentication and everything on it -- budget names,
// allocations, spend, which models are being used and what they cost -- is exactly what
// an operator would not want served to their network.
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		listen       = fs.String("listen", dashboard.DefaultListen, "address to bind; loopback by default because the dashboard has no authentication")
		activityPath = fs.String("activity", defaultActivityPath(), "path to the activity database; request history is omitted if it does not exist")
		limit        = fs.Int("limit", 0, "rows in the recent-request table; zero uses the default")
	)
	dbPath := dbFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Ctrl-C and SIGTERM both mean "stop serving", and the difference between them does
	// not matter to a process that holds no locks and owes no cleanup beyond closing two
	// databases.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, store, err := open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	// A missing activity database is not created here. Opening it would materialize an
	// empty store as a side effect of looking at a budget, and the dashboard would then
	// report "no requests recorded" -- which reads as a measurement -- instead of "no
	// request history is available", which is the actual situation.
	var acts report.Activity
	switch _, statErr := os.Stat(*activityPath); {
	case statErr == nil:
		actStore, err := activitysqlite.Open(ctx, *activityPath)
		if err != nil {
			return fmt.Errorf("open activity database: %w", err)
		}
		defer actStore.Close()
		acts = actStore
	case errors.Is(statErr, os.ErrNotExist):
		fmt.Fprintf(os.Stderr, "throttle: no activity database at %s, so request history, "+
			"breakdowns, and per-request detail will be unavailable.\n"+
			"  Budget figures come from the ledger and are unaffected.\n", *activityPath)
	default:
		return fmt.Errorf("stat activity database: %w", statErr)
	}

	rep, err := report.New(report.Config{Ledger: store, Activity: acts})
	if err != nil {
		return err
	}

	srv, err := dashboard.New(dashboard.Config{
		Reporter:      rep,
		Version:       version,
		ActivityLimit: *limit,
	})
	if err != nil {
		return err
	}

	// Bind before announcing. Printing a URL for a listener that failed to open sends the
	// reader to a dead page and hides the real error underneath it.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	defer ln.Close()

	if warning := dashboard.ExposureWarning(*listen); warning != "" {
		fmt.Fprintln(os.Stderr, "throttle: "+warning)
	}
	fmt.Printf("throttle dashboard on http://%s/\n", displayAddr(ln.Addr()))
	fmt.Println("read-only: no request served by this process moves money. Ctrl-C to stop.")

	// Serve owns the timeouts and the graceful shutdown. Restating them here would be a
	// second set of numbers to keep in step with the first.
	err = srv.Serve(ctx, ln)

	// A cancelled context is how Ctrl-C arrives, and a clean stop is not a failure.
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	fmt.Println("stopped")
	return nil
}

// displayAddr renders the bound address for a clickable line.
//
// A port of zero asked the kernel to choose, so the announced URL has to come from the
// listener rather than from the flag; and an all-interfaces bind is shown as loopback
// because that is the address the person reading the terminal can actually use.
func displayAddr(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	// JoinHostPort brackets an IPv6 literal itself.
	return net.JoinHostPort(host, port)
}
