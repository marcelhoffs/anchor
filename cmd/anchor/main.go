// Command anchor is a self-hosted, multi-user bookmark manager.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/config"
	"github.com/marcelhoffs/anchor/internal/db"
	"github.com/marcelhoffs/anchor/internal/fetch"
	"github.com/marcelhoffs/anchor/internal/jobs"
	"github.com/marcelhoffs/anchor/internal/store"
	"github.com/marcelhoffs/anchor/internal/web"
)

const usage = `Usage: anchor <command> [arguments]

Commands:
  serve                          run the web server (runs migrations first)
  migrate                        apply database migrations and exit
  user create <name> [--admin]   create a user; prints a generated password,
                                 or reads one from stdin with --password-stdin
  user reset-password <name>     set a new password and sign out all sessions
  user reset-2fa <name>          remove all second factors and sign out all sessions
  user unlink-sso <name>         unlink single sign-on, for a lost provider account
  backup <file>                  write a consistent snapshot of the database
  healthcheck                    exit 0 if the local server answers /healthz
  version                        print the version

Configuration is read from ANCHOR_* environment variables.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = withDB(ctx, func(*db.DB) error { return nil })
	case "user":
		err = userCmd(ctx, args)
	case "backup":
		err = backup(ctx, args)
	case "healthcheck":
		err = healthcheck()
	case "version", "--version":
		fmt.Println(web.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "anchor: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "anchor:", err)
		os.Exit(1)
	}
}

// withDB opens and migrates the database, then runs fn.
func withDB(ctx context.Context, fn func(*db.DB) error) error {
	cfg, err := config.Load(false)
	if err != nil {
		return err
	}
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	return fn(d)
}

func openDB(ctx context.Context, cfg *config.Config) (*db.DB, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	d, err := db.Open(filepath.Join(cfg.DataDir, "anchor.db"))
	if err != nil {
		return nil, err
	}
	if err := d.Migrate(ctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

func serve(ctx context.Context) error {
	cfg, err := config.Load(true)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	st := store.New(d)

	if err := seedAdmin(ctx, cfg, st, log); err != nil {
		return err
	}
	go cleanSessions(ctx, st, log)
	worker := jobs.NewWorker(st, log)
	go worker.Run(ctx)
	fetcher := jobs.NewFetcher(st, fetch.New(cfg.Fetch == "all"), log)
	fetcher.IconServices = cfg.IconFallback
	if cfg.Fetch != "off" {
		go fetcher.Run(ctx)
	}

	srv, err := web.New(cfg, st, log, worker, fetcher)
	if err != nil {
		return err
	}
	go srv.Run(ctx)
	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "base_url", cfg.BaseURL.String())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdownCtx)
}

// seedAdmin creates the first admin from ANCHOR_ADMIN_USER and
// ANCHOR_ADMIN_PASSWORD when the database has no users yet.
func seedAdmin(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) error {
	if cfg.AdminUser == "" && cfg.AdminPassword == "" {
		return nil
	}
	if err := auth.ValidateUsername(cfg.AdminUser); err != nil {
		return fmt.Errorf("ANCHOR_ADMIN_USER: %w", err)
	}
	if err := auth.ValidatePassword(cfg.AdminPassword); err != nil {
		return fmt.Errorf("ANCHOR_ADMIN_PASSWORD: %w", err)
	}
	u, err := st.CreateFirstAdmin(ctx, cfg.AdminUser, auth.HashPassword(cfg.AdminPassword))
	if errors.Is(err, store.ErrConflict) {
		return nil // users already exist; the variables only seed an empty database
	}
	if err != nil {
		return err
	}
	log.Info("admin created from environment", "user", u.Username)
	return nil
}

func cleanSessions(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.DeleteExpiredSessions(ctx); err != nil && ctx.Err() == nil {
			log.Error("clean sessions", "err", err)
		} else if n > 0 {
			log.Debug("expired sessions removed", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func userCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: anchor user create|reset-password|reset-2fa|unlink-sso <name>")
	}
	fs := flag.NewFlagSet("user "+args[0], flag.ContinueOnError)
	admin := fs.Bool("admin", false, "make the user an administrator")
	fromStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin")
	// Accept flags before or after the username.
	var name string
	rest := args[1:]
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if rest = fs.Args(); len(rest) > 0 {
			if name != "" {
				return fmt.Errorf("unexpected argument %q", rest[0])
			}
			name, rest = rest[0], rest[1:]
		}
	}
	if err := auth.ValidateUsername(name); err != nil {
		return err
	}
	password, generated := auth.RandomPassword(), true
	if *fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read password: %w", err)
		}
		password, generated = strings.TrimRight(line, "\r\n"), false
		if err := auth.ValidatePassword(password); err != nil {
			return err
		}
	}

	return withDB(ctx, func(d *db.DB) error {
		st := store.New(d)
		switch args[0] {
		case "create":
			u, err := st.CreateUser(ctx, name, auth.HashPassword(password), *admin)
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("user %q already exists", name)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Created user %s (admin: %v).\n", u.Username, u.IsAdmin)
		case "reset-password":
			u, err := st.UserByUsername(ctx, name)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no user %q", name)
			}
			if err != nil {
				return err
			}
			if err := st.SetPassword(ctx, u.ID, auth.HashPassword(password), nil); err != nil {
				return err
			}
			fmt.Printf("Password for %s reset; all their sessions were signed out.\n", u.Username)
		case "reset-2fa":
			u, err := st.UserByUsername(ctx, name)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no user %q", name)
			}
			if err != nil {
				return err
			}
			if err := st.ResetMFA(ctx, u.ID); err != nil {
				return err
			}
			fmt.Printf("Two-factor authentication for %s removed; all their sessions were signed out.\n", u.Username)
			return nil
		case "unlink-sso":
			u, err := st.UserByUsername(ctx, name)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no user %q", name)
			}
			if err != nil {
				return err
			}
			if err := st.UnlinkIdentity(ctx, u.ID, true); errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%s has no single sign-on linked", u.Username)
			} else if err != nil {
				return err
			}
			fmt.Printf("Single sign-on unlinked from %s.\n", u.Username)
			if !u.HasPassword() {
				fmt.Printf("They have no password: set one with anchor user reset-password %s.\n", u.Username)
			}
			return nil
		default:
			return fmt.Errorf("unknown user command %q", args[0])
		}
		if generated {
			fmt.Printf("Password: %s\n", password)
		}
		return nil
	})
}

func backup(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: anchor backup <file>")
	}
	if _, err := os.Stat(args[0]); err == nil {
		return fmt.Errorf("%s already exists", args[0])
	}
	// A folder for backups, such as /data/backups, needn't exist yet.
	if err := os.MkdirAll(filepath.Dir(args[0]), 0o750); err != nil {
		return err
	}
	return withDB(ctx, func(d *db.DB) error {
		if err := d.Backup(ctx, args[0]); err != nil {
			return err
		}
		fmt.Println("Backup written to", args[0])
		return nil
	})
}

func healthcheck() error {
	cfg, err := config.Load(false)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", res.Status)
	}
	return nil
}
