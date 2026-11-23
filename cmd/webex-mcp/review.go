package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mythingies/plugin-webex/internal/replydesk"
	"github.com/mythingies/plugin-webex/internal/webex"
)

// reviewOptions are the --review flags.
type reviewOptions struct {
	dryRun    bool
	noBrowser bool
	lookback  time.Duration
	drafter   string
	model     string
}

// parseReviewArgs parses the arguments after --review.
func parseReviewArgs(args []string, stderr io.Writer) (reviewOptions, error) {
	opts := reviewOptions{lookback: replydesk.DefaultLookback}
	fs := flag.NewFlagSet("webex-mcp --review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&opts.dryRun, "dry-run", false, "list unanswered direct messages and exit; no drafting, no UI")
	fs.BoolVar(&opts.noBrowser, "no-browser", false, "print the review URL instead of opening a browser")
	lookback := fs.String("lookback", "7d", "how far back to look for unanswered messages (e.g. 36h, 7d; 1h to 90d)")
	fs.StringVar(&opts.drafter, "drafter", replydesk.ModeAuto, "drafter mode: auto, bare, isolated, or off")
	fs.StringVar(&opts.model, "model", "", "model for the drafter (passed to claude --model)")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	d, err := parseLookback(*lookback)
	if err != nil {
		return opts, err
	}
	opts.lookback = d
	switch opts.drafter {
	case replydesk.ModeAuto, replydesk.ModeBare, replydesk.ModeIsolated, replydesk.ModeOff:
	default:
		return opts, fmt.Errorf("invalid --drafter %q (want auto, bare, isolated, or off)", opts.drafter)
	}
	return opts, nil
}

// parseLookback accepts Go durations plus a "d" (days) suffix, between 1h
// and 90 days.
func parseLookback(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid --lookback %q", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("invalid --lookback %q", s)
		}
	}
	if d < time.Hour || d > 90*24*time.Hour {
		return 0, fmt.Errorf("--lookback must be between 1h and 90d, got %s", s)
	}
	return d, nil
}

// closeTimeout bounds how long shutdown waits for background drafts to stop
// and their temp dirs to be removed.
const closeTimeout = 10 * time.Second

// alreadyOpenMessage is shown when another --review process holds the store.
const alreadyOpenMessage = "Reply Desk is already open in another window. Use that window, or close it and try again."

// handleReview runs Reply Desk: the loopback review UI, or with --dry-run a
// plain list of unanswered direct messages.
func handleReview(args []string) {
	opts, err := parseReviewArgs(args, os.Stderr)
	if err != nil {
		if err == flag.ErrHelp {
			return
		}
		log.Fatalf("review: %v", err)
	}

	client := webex.NewClient(resolveAuth())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err = runReview(ctx, client, opts)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, reviewFailure(err))
		os.Exit(1)
	}
}

// reviewFailure is the line printed when --review fails.
func reviewFailure(err error) string {
	if errors.Is(err, replydesk.ErrAlreadyRunning) {
		return alreadyOpenMessage
	}
	return "review: " + err.Error()
}

// runReview runs --review with the parsed options. It returns rather than
// exiting so the service is always closed before the process ends.
func runReview(ctx context.Context, client *webex.Client, opts reviewOptions) error {
	if opts.dryRun {
		return reviewDryRun(ctx, client, opts.lookback, os.Stdout, os.Stderr)
	}

	storePath, err := replydesk.DefaultStorePath()
	if err != nil {
		return err
	}
	drafter := replydesk.NewDrafter(replydesk.DrafterConfig{Mode: opts.drafter, Model: opts.model})
	svc, err := replydesk.NewService(replydesk.Config{
		API:       client,
		StorePath: storePath,
		Drafter:   drafter,
		Lookback:  opts.lookback,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Reply Desk: drafter mode %q, looking back %s.\n", drafter.Mode(), opts.lookback)
	serve := func(ctx context.Context) error {
		return replydesk.Serve(ctx, svc, replydesk.ServeOptions{OpenBrowser: !opts.noBrowser})
	}
	return serveThenClose(ctx, serve, svc.Close, closeTimeout)
}

// serveThenClose runs serve and then, whatever it returned, closeFn with a
// fresh timeout. ctx is usually cancelled by then (Ctrl-C), and Close still
// needs time to stop drafts and remove their temp dirs.
func serveThenClose(ctx context.Context, serve, closeFn func(context.Context) error, timeout time.Duration) error {
	serveErr := serve(ctx)
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	var closeErr error
	if err := closeFn(cctx); err != nil {
		closeErr = fmt.Errorf("closing Reply Desk: %w", err)
	}
	return errors.Join(serveErr, closeErr)
}

// reviewDryRun prints the pending bursts without drafting or serving. Rooms
// that failed are a warning on errOut. It fails only when the list can't be
// trusted: the scan was rate limited or interrupted (after printing what it
// found), or nothing is pending and the room listing itself failed.
func reviewDryRun(ctx context.Context, api replydesk.API, lookback time.Duration, out, errOut io.Writer) error {
	me, err := api.GetMe()
	if err != nil {
		return fmt.Errorf("identifying you (people/me): %w", err)
	}
	if me == nil || me.ID == "" {
		return errors.New("identifying you (people/me): no person ID returned")
	}
	now := time.Now()
	pending, scanErr := replydesk.Scan(ctx, api, me.ID, lookback, now)
	if scanErr == nil {
		return writeDryRun(out, pending, now)
	}
	fatal := dryRunFatal(ctx, scanErr, len(pending))
	if fatal && len(pending) == 0 {
		// "No unanswered direct messages." would be a false all-clear.
		return scanErr
	}
	if err := writeDryRun(out, pending, now); err != nil {
		return err
	}
	if fatal {
		return fmt.Errorf("scan stopped early, so the list above may be incomplete: %w", scanErr)
	}
	_, err = io.WriteString(errOut, "warning: some rooms could not be scanned: "+terminalSafe(scanErr.Error())+"\n")
	return err
}

// dryRunFatal reports whether a Scan error makes the result untrustworthy:
// a rate limit or interruption stopped the scan, or nothing is pending and
// the error is more than failed rooms (the room listing itself failed).
func dryRunFatal(ctx context.Context, err error, pending int) bool {
	if err == nil {
		return false
	}
	var rl *webex.RateLimitError
	if errors.As(err, &rl) || ctx.Err() != nil {
		return true
	}
	return pending == 0 && !onlyRoomErrors(err)
}

// onlyRoomErrors reports whether err consists only of per-room failures.
// replydesk.Scan joins those, each prefixed "room <id>: ", while a failed
// room listing is prefixed "listing direct rooms: ". replydesk exposes no
// typed error for the distinction.
func onlyRoomErrors(err error) bool {
	errs := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	}
	if len(errs) == 0 {
		return false
	}
	for _, e := range errs {
		if e == nil || !strings.HasPrefix(e.Error(), "room ") {
			return false
		}
	}
	return true
}

// writeDryRun renders pending bursts for a terminal in a single write.
// Sender-controlled text (room titles, reasons, message text) goes through
// terminalSafe.
func writeDryRun(out io.Writer, pending []replydesk.Pending, now time.Time) error {
	var b strings.Builder
	if len(pending) == 0 {
		b.WriteString("No unanswered direct messages.\n")
	} else {
		fmt.Fprintf(&b, "%d direct message conversation(s) awaiting your reply:\n", len(pending))
		for _, p := range pending {
			fmt.Fprintf(&b, "\n%s (%s ago)\n  %s\n", terminalSafe(p.RoomTitle), humanDuration(now.Sub(p.HeadCreated)), terminalSafe(p.Reason))
			if len(p.Flags) > 0 {
				fmt.Fprintf(&b, "  flags: %s\n", terminalSafe(strings.Join(p.Flags, ", ")))
			}
			for _, m := range p.Burst {
				fmt.Fprintf(&b, "  > %s\n", preview(m.Text, 120))
			}
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// preview flattens text to one terminal-safe line of at most n runes.
func preview(s string, n int) string {
	s = terminalSafe(s)
	if s == "" {
		return "[no text]"
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// terminalSafe maps control characters (C0, DEL, and C1, so ESC and every
// terminal escape sequence lose their introducer), line and paragraph
// separators, and bidi controls to spaces, then collapses whitespace. The
// result is one line that cannot move the cursor, recolor or retitle the
// terminal, or reorder what the user reads.
func terminalSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || isBidiOrSeparator(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// isBidiOrSeparator reports the format characters terminalSafe removes:
// U+2028 and U+2029, and the bidi marks, embeddings, overrides, and isolates.
func isBidiOrSeparator(r rune) bool {
	switch {
	case r == 0x2028, r == 0x2029: // line and paragraph separators
		return true
	case r == 0x200E, r == 0x200F, r == 0x061C: // LRM, RLM, ALM
		return true
	case r >= 0x202A && r <= 0x202E: // LRE, RLE, PDF, LRO, RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	}
	return false
}

// humanDuration renders a duration the way people say it: "45m", "3h31m",
// "2d5h", "7d".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	days, rem := d/(24*time.Hour), d%(24*time.Hour)
	hours, mins := rem/time.Hour, (rem%time.Hour)/time.Minute
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
