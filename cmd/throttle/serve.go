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

	"throttle/config"
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
//
// The address may now come from a config file, and that changes nothing about the warning: a
// non-loopback bind is exposed however it was requested, and a warning that only fired for
// the flag would be a warning that stops firing the moment somebody makes the setting
// permanent -- which is exactly when it matters most.
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		listen = fs.String("listen", "", "address to bind; loopback by default because the dashboard has no authentication")
		limit  = fs.Int("limit", 0, "rows in the recent-request table; zero uses the configured default")
	)
	common := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.load(func(over *config.Overrides) {
		setIfPassedName(fs, "listen", func() { over.Listen = listen })
		setIfPassedName(fs, "limit", func() { over.ActivityLimit = limit })
	})
	if err != nil {
		return err
	}

	// Ctrl-C and SIGTERM both mean "stop serving", and the difference between them does
	// not matter to a process that holds no locks and owes no cleanup beyond closing two
	// databases.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	// A missing activity database is not created here. Opening it would materialize an
	// empty store as a side effect of looking at a budget, and the dashboard would then
	// report "no requests recorded" -- which reads as a measurement -- instead of "no
	// request history is available", which is the actual situation.
	acts, closeActs, err := openActivityIfPresent(ctx, cfg.Activity)
	if err != nil {
		return err
	}
	defer closeActs()
	if acts == nil {
		fmt.Fprintf(os.Stderr, "throttle: no activity database at %s, so request history, "+
			"breakdowns, and per-request detail will be unavailable.\n"+
			"  Budget figures come from the ledger and are unaffected.\n", cfg.Activity)
	}

	rep, err := report.New(report.Config{Ledger: store, Activity: acts})
	if err != nil {
		return err
	}

	srv, err := dashboard.New(dashboard.Config{
		Reporter:      rep,
		Version:       version,
		ActivityLimit: cfg.ActivityLimit,
	})
	if err != nil {
		return err
	}

	// Warned on the resolved address, whatever set it, and before the socket exists rather
	// than after: a warning that arrives once the port is already open is a warning about
	// something that has already happened. The origin is named because a surprising bind is
	// usually a config file somebody forgot about, and "it is in your config file" is not
	// enough to find it.
	if warning := dashboard.ExposureWarning(cfg.Listen); warning != "" {
		fmt.Fprintln(os.Stderr, "throttle: "+warning)
		if cfg.Path != "" && cfg.ListenFromFile() {
			fmt.Fprintf(os.Stderr, "  This address came from dashboard.listen in %s.\n", cfg.Path)
		}
	}

	// Bind before announcing. Printing a URL for a listener that failed to open sends the
	// reader to a dead page and hides the real error underneath it.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	defer ln.Close()
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
