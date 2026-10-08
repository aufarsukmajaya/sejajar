// sejajar: compare every site's running commit + DB schema against the
// GitLab branch head, and fan out one deploy pipeline per site at one SHA.
// Run as a web dashboard (serve) or from the terminal (status, deploy).
package main

import (
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
	only := flag.String("sites", "", "status/deploy: comma-separated site names (default: all)")
	yes := flag.Bool("yes", false, "deploy: actually trigger pipelines (default is dry run)")
	addr := flag.String("addr", ":8090", "serve: listen address")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: sejajar [flags] <command>

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
	var names []string
	if *only != "" {
		names = strings.Split(*only, ",")
	}

	switch flag.Arg(0) {
	case "serve":
		runServer(ctx, f, *addr)
	case "status":
		printStatus(ctx, f, names)
	case "deploy":
		runDeploy(ctx, f, names, *yes)
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

	s := &server{fleet: f, store: f.store}
	go s.poll(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(f),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute, // a deploy fans out one GitLab call per site
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Printf("sejajar listening on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fail(err)
	}
}

func importConfig(ctx context.Context, store *Store, path string) {
	if path == "" {
		fail(errors.New("usage: sejajar import <sites.json>"))
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

func printStatus(ctx context.Context, f *Fleet, names []string) {
	snap, err := f.Snapshot(ctx)
	if err != nil {
		fail(err)
	}
	want := map[string]bool{}
	for _, n := range names {
		want[strings.TrimSpace(n)] = true
	}
	for n := range want {
		if !slices.ContainsFunc(snap.Sites, func(s SiteStatus) bool { return s.Name == n }) {
			fail(fmt.Errorf("unknown site %q", n))
		}
	}

	fmt.Printf("%s head: %s\n\n", snap.Branch, short(snap.Head))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE\tSTATE\tCOMMIT\tBEHIND\tSCHEMA(db)\tPENDING\tUNKNOWN\tERROR")
	bad := false
	for _, s := range snap.Sites {
		if len(want) > 0 && !want[s.Name] {
			continue
		}
		bad = bad || s.State != "OK"
		if s.Schema == nil {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\t-\t%s\n", s.Name, s.State, s.Error)
			continue
		}
		behind := "?"
		if s.Behind >= 0 {
			behind = fmt.Sprint(s.Behind)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t\n", s.Name, s.State, short(s.Commit), behind, s.Schema.DB, len(s.Schema.Pending), len(s.Schema.Unknown))
	}
	tw.Flush()
	if bad {
		os.Exit(1) // usable as a CI gate
	}
}

// runDeploy is the terminal deploy, saved to the same history the dashboard shows.
func runDeploy(ctx context.Context, f *Fleet, names []string, yes bool) {
	cfg, gl, err := f.load(ctx)
	if err != nil {
		fail(err)
	}
	sites, err := pick(cfg.Sites, names)
	if err != nil {
		fail(err)
	}
	head, err := gl.branchHead(cfg.GitLab.Branch)
	if err != nil {
		fail(err)
	}
	fmt.Printf("deploy %s@%s to %d site(s)\n", cfg.GitLab.Branch, short(head), len(sites))
	if !yes {
		for _, s := range sites {
			fmt.Printf("  would trigger: SITE=%s DEPLOY_SHA=%s\n", s.Name, head)
		}
		fmt.Println("dry run; pass -yes to trigger")
		return
	}
	d, err := f.Deploy(ctx, "cli", names, head, "")
	if err != nil {
		fail(err)
	}
	if err := f.store.AddDeploy(ctx, d); err != nil {
		fmt.Fprintln(os.Stderr, "warning: triggered, but not saved to history:", err)
	}
	failed := 0
	for _, r := range d.Runs {
		if r.Error != "" {
			failed++
			fmt.Printf("  %-20s FAILED %s\n", r.Site, r.Error)
			continue
		}
		fmt.Printf("  %-20s %s\n", r.Site, r.WebURL)
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
	fmt.Fprintln(os.Stderr, "sejajar:", err)
	os.Exit(1)
}
