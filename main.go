// resonate: compare every site's running commit + DB schema against the
// GitLab branch head, and fan out one deploy pipeline per site at one SHA.
// Run as a web dashboard (serve) or from the terminal (status, deploy).
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

func main() {
	onlySites := flag.String("sites", "", "status/deploy: comma-separated site names (default: all)")
	onlyServices := flag.String("services", "", "status/deploy: comma-separated services, name+postfix (default: all; deploy skips jobs unless named)")
	yes := flag.Bool("yes", false, "deploy: actually trigger pipelines (default is dry run)")
	addr := flag.String("addr", ":8090", "serve: listen address")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: resonate [flags] <command>

  serve                   dashboard + API; settings and sites are edited in the dashboard
  status                  print every site's state; exits 1 if any is not OK
  deploy                  trigger one pipeline per site at the branch head (dry run without -yes)
  import <sites.json>     replace settings and sites from a file (see sites.example.json)
  reset-password [pw]     set the local admin password (random if omitted), print it, and
                          turn GitLab sign-in off so it works (break-glass)

env: DATABASE_URL (all commands), GITLAB_TOKEN (serve, status, deploy)`)
		flag.PrintDefaults()
	}
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fail(errors.New("DATABASE_URL is not set"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := openStore(ctx, dsn)
	if err != nil {
		fail(err)
	}
	defer store.Close()

	switch flag.Arg(0) {
	case "import":
		importConfig(ctx, store, flag.Arg(1))
		return
	case "reset-password":
		pw := flag.Arg(1)
		if pw == "" {
			pw = newPassword()
		}
		setPassword(ctx, store, pw)
		if err := store.ClearOAuth(ctx); err != nil {
			fail(err)
		}
		fmt.Println("admin password:", pw)
		fmt.Println("GitLab sign-in is off until the application id is set again in Settings")
		return
	}

	token := os.Getenv("GITLAB_TOKEN")
	if token == "" {
		fail(errors.New("GITLAB_TOKEN is not set (needs api scope)"))
	}
	f := &Fleet{store: store, token: token}
	sites, services := splitList(*onlySites), splitList(*onlyServices)

	switch flag.Arg(0) {
	case "serve":
		runServer(ctx, f, *addr)
	case "status":
		printStatus(ctx, f, sites, services)
	case "deploy":
		runDeploy(ctx, f, sites, services, *yes)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func runServer(ctx context.Context, f *Fleet, addr string) {
	// First run: there is no password yet, so make one and show it once.
	if user, hash, err := f.store.GetLogin(ctx); err != nil {
		fail(err)
	} else if hash == "" {
		pw := newPassword()
		setPassword(ctx, f.store, pw)
		log.Printf("first run: sign in as %s / %s, then set up GitLab sign-in in Settings", user, pw)
	}

	s := newServer(f)
	go s.poll(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute, // a deploy fans out one GitLab call per site
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Printf("resonate listening on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fail(err)
	}
}

func importConfig(ctx context.Context, store *Store, path string) {
	if path == "" {
		fail(errors.New("usage: resonate import <sites.json>"))
	}
	cfg, err := readConfigFile(path)
	if err != nil {
		fail(err)
	}
	if err := store.SaveConfig(ctx, &cfg.Config, cfg.VersionToken, cfg.OAuthSecret, "import"); err != nil {
		fail(err)
	}
	fmt.Printf("imported %s: %d site(s)\n", path, len(cfg.Sites))
}

func setPassword(ctx context.Context, store *Store, pw string) {
	user, _, err := store.GetLogin(ctx)
	if err != nil {
		fail(err)
	}
	hash, err := hashPassword(pw)
	if err == nil {
		err = store.SetLogin(ctx, user, hash)
	}
	if err != nil {
		fail(err)
	}
}

func splitList(s string) []string {
	var out []string
	for x := range strings.SplitSeq(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// printStatus prints one line per (site, service) cell; it exits 1 when any
// shown cell (jobs only when named) is not OK, so CI can use it as a gate.
func printStatus(ctx context.Context, f *Fleet, sites, services []string) {
	snap, cfg, err := f.snapshot(ctx)
	if err != nil {
		fail(err)
	}
	if _, err := cfg.targetsFor(sites, services); err != nil {
		fail(err)
	}

	fmt.Printf("%s head: %s\n\n", snap.Branch, short(snap.Head))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE\tSERVICE\tKIND\tSTATE\tCOMMIT\tBEHIND\tFROM\tSCHEMA(db)\tPENDING\tUNKNOWN\tERROR")
	bad := false
	for _, c := range snap.Cells {
		if len(sites) > 0 && !slices.Contains(sites, c.Site) || len(services) > 0 && !slices.Contains(services, c.Service+c.Postfix) {
			continue
		}
		// Jobs are one-off, like in deploy: they only gate when named.
		if c.Kind != "job" || slices.Contains(services, c.Service+c.Postfix) {
			bad = bad || c.State != "OK"
		}
		behind, db, pending, unknown := "?", "-", "-", "-"
		if c.Behind >= 0 {
			behind = fmt.Sprint(c.Behind)
		}
		if c.Schema != nil {
			db, pending, unknown = c.Schema.DB, fmt.Sprint(len(c.Schema.Pending)), fmt.Sprint(len(c.Schema.Unknown))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Site, c.Service+c.Postfix, c.Kind, c.State,
			cmp.Or(short(c.Commit), "-"), behind, c.Source, db, pending, unknown, c.Error)
	}
	tw.Flush()
	if bad {
		os.Exit(1) // usable as a CI gate
	}
}

// runDeploy is the terminal deploy, saved to the same history the dashboard shows.
func runDeploy(ctx context.Context, f *Fleet, sites, services []string, yes bool) {
	cfg, gl, err := f.load(ctx)
	if err != nil {
		fail(err)
	}
	targets, err := cfg.targetsFor(sites, services)
	if err != nil {
		fail(err)
	}
	head, err := gl.branchHead(ctx, cfg.GitLab.Branch)
	if err != nil {
		fail(err)
	}
	fmt.Printf("deploy %s@%s: %d pipeline(s)\n", cfg.GitLab.Branch, short(head), len(targets))
	if !yes {
		for _, t := range targets {
			fmt.Printf("  would trigger: %s %s\n", t.Site, t.Service+t.Postfix)
		}
		fmt.Println("dry run; pass -yes to trigger")
		return
	}
	d, err := f.Deploy(ctx, "cli", targets, head, "")
	if err != nil {
		fail(err)
	}
	if err := f.store.AddDeploy(ctx, d); err != nil {
		fmt.Fprintln(os.Stderr, "warning: triggered, but not saved to history:", err)
	}
	failed := 0
	for _, r := range d.Runs {
		name := r.Site + " " + r.Service + r.Postfix
		if r.Error != "" {
			failed++
			fmt.Printf("  %-40s FAILED %s\n", name, r.Error)
			continue
		}
		fmt.Printf("  %-40s %s\n", name, r.WebURL)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "resonate:", err)
	os.Exit(1)
}
