package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

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
	defer stop()

	if opts.dryRun {
		if err := reviewDryRun(ctx, client, opts.lookback, os.Stdout); err != nil {
			log.Fatalf("review: %v", err)
		}
		return
	}

	storePath, err := replydesk.DefaultStorePath()
	if err != nil {
		log.Fatalf("review: %v", err)
	}
	drafter := replydesk.NewDrafter(replydesk.DrafterConfig{Mode: opts.drafter, Model: opts.model})
	svc, err := replydesk.NewService(replydesk.Config{
		API:       client,
		StorePath: storePath,
		Drafter:   drafter,
		Lookback:  opts.lookback,
	})
	if err != nil {
		log.Fatalf("review: %v", err)
	}
	fmt.Fprintf(os.Stderr, "Reply Desk: drafter mode %q, looking back %s.\n", drafter.Mode(), opts.lookback)
	if err := replydesk.Serve(ctx, svc, replydesk.ServeOptions{OpenBrowser: !opts.noBrowser}); err != nil {
		log.Fatalf("review: %v", err)
	}
}

// reviewDryRun prints the pending bursts without drafting or serving.
func reviewDryRun(ctx context.Context, client *webex.Client, lookback time.Duration, out io.Writer) error {
	me, err := client.GetMe()
	if err != nil {
		return fmt.Errorf("identifying you (people/me): %w", err)
	}
	now := time.Now()
	pending, err := replydesk.Scan(ctx, client, me.ID, lookback, now)
	if err != nil && len(pending) == 0 {
		return err
	}
	writeDryRun(out, pending, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: some rooms could not be scanned: %v\n", err)
	}
	return nil
}

// writeDryRun renders pending bursts for a terminal.
func writeDryRun(out io.Writer, pending []replydesk.Pending, now time.Time) {
	if len(pending) == 0 {
		fmt.Fprintln(out, "No unanswered direct messages.")
		return
	}
	fmt.Fprintf(out, "%d direct message conversation(s) awaiting your reply:\n", len(pending))
	for _, p := range pending {
		fmt.Fprintf(out, "\n%s (%s ago)\n  %s\n", p.RoomTitle, now.Sub(p.HeadCreated).Round(time.Minute), p.Reason)
		if len(p.Flags) > 0 {
			fmt.Fprintf(out, "  flags: %s\n", strings.Join(p.Flags, ", "))
		}
		for _, m := range p.Burst {
			fmt.Fprintf(out, "  > %s\n", preview(m.Text, 120))
		}
	}
}

// preview flattens text to one line of at most n runes.
func preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "[no text]"
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
