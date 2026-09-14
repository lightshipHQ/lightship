// Command lightship is the control plane.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lightshipHQ/lightship/internal/auth"
	"github.com/lightshipHQ/lightship/internal/httpapi"
	"github.com/lightshipHQ/lightship/internal/localmcp"
	"github.com/lightshipHQ/lightship/internal/model"
	"github.com/lightshipHQ/lightship/internal/store"
	"github.com/lightshipHQ/lightship/internal/traces"

	"golang.org/x/term"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "mcp":
		err = runLocalMCP()
	case "check":
		err = check()
	case "hash":
		err = hash()
	default:
		err = errors.New("usage: lightship <serve|mcp|check|hash>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runLocalMCP is the analysis companion a coding client launches over stdio. Its environment is
// process-local, so the bearer key need not appear in tool arguments or protocol messages.
func runLocalMCP() error {
	bridge, err := localmcp.New(localmcp.Config{
		BaseURL:   os.Getenv("LIGHTSHIP_URL"),
		APIKey:    os.Getenv("LIGHTSHIP_API_KEY"),
		ExportDir: os.Getenv("LIGHTSHIP_EXPORT_DIR"),
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return bridge.Run(ctx, os.Stdin, os.Stdout)
}

// check compiles what is stored. An invalid model should never be stored — every write compiles
// first — so this failing means something wrote around the API, and it is the one thing worth
// running against a deployment you did not configure yourself.
func check() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	m, err := st.LoadModel(ctx)
	if err != nil {
		return err
	}
	if _, err := model.Compile(m); err != nil {
		return err
	}
	if m.Binding.Table == "" {
		fmt.Printf("model version %d compiles, but no source binding is set: queries will fail "+
			"closed until one is\n", m.Version)
		return nil
	}
	fmt.Printf("ok: version %d, table %s, %d fields, %d roles\n",
		m.Version, m.Binding.Table, len(m.Fields), len(m.Roles))
	return nil
}

// hash prompts when attached to a terminal and reads stdin otherwise, so it works both by hand
// and in a provisioning script.
func hash() error {
	var pw []byte
	var err error
	if term.IsTerminal(int(syscall.Stdin)) {
		fmt.Fprint(os.Stderr, "password: ")
		pw, err = term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
	} else {
		pw, err = io.ReadAll(io.LimitReader(os.Stdin, 4<<10))
		pw = bytes.TrimRight(pw, "\r\n")
	}
	if err != nil {
		return err
	}
	if len(pw) == 0 {
		return errors.New("empty password")
	}
	h, err := auth.Hash(string(pw))
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadServeConfig()
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	applied, err := st.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		log.Info("migrations applied", "files", applied)
	}
	generated, err := st.EnsureAdmin(ctx, cfg.AdminPasswordHash)
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	switch {
	case generated != "":
		printBootstrapCredential(generated, cfg.Address)
	case cfg.AdminPasswordHash == "":
		// The recovery path for a lost generated credential has to be discoverable from the place
		// an operator will actually look, which is this log.
		log.Info("admin credential already provisioned; to rotate or recover it, set " +
			"LIGHTSHIP_ADMIN_PASSWORD_HASH (generate one with `lightship hash`) and restart")
	}

	models := model.New(st, cfg.ModelTTL)
	// Load once at startup so a broken stored model is loud here rather than on someone's first
	// query. A fresh deployment with no binding is not broken — it is waiting to be set up.
	m, err := models.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("load access model: %w", err)
	}
	if err := m.Ready(); err != nil {
		log.Warn("no usable source binding yet; queries fail closed until one is set", "err", err)
	} else {
		log.Info("access model loaded", "version", m.Schema.Version, "table", m.Schema.Binding.Table,
			"fields", len(m.Schema.Fields), "roles", len(m.Schema.Roles))
	}

	// ClickHouse is not a boot dependency. If it is unreachable the control plane still starts,
	// still serves /healthz and still writes the audit log; queries fail closed, which is the
	// behaviour we want, and refusing to boot would take the audit trail down with the source.
	reader, err := traces.Open(cfg.ClickHouseDSN)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := reader.Ping(ctx); err != nil {
		log.Warn("trace source unreachable at startup; queries will fail until it returns", "err", err)
	}

	go pruneExpired(ctx, st, cfg.AuditRetention, log)

	srv := &http.Server{
		Addr:              cfg.Address,
		Handler:           httpapi.New(st, reader, models, log, cfg.serverOptions()...),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", cfg.Address, "model_ttl", cfg.ModelTTL,
		"demo_mode", cfg.Demo != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// printBootstrapCredential writes the generated admin password straight to stderr, deliberately
// not through slog: it is not a log event, and a structured-log pipeline that reformats or drops
// it should not get the chance. It is printed exactly once, on the boot that generated it — a
// restart never reprints, and the line survives in `docker compose logs` until the container is
// removed.
func printBootstrapCredential(password, addr string) {
	host := addr
	if strings.HasPrefix(addr, ":") {
		host = "localhost" + addr
	}
	fmt.Fprintln(os.Stderr, strings.Join([]string{
		"─────────────────────────────────────────────────────────────",
		"  bootstrap admin credential — shown once, only the hash is stored",
		"",
		"    username  admin",
		"    password  " + password,
		"",
		"  sign in at http://" + host + "/ to set up your trace source.",
		"  lost it? set LIGHTSHIP_ADMIN_PASSWORD_HASH and restart.",
		"─────────────────────────────────────────────────────────────",
	}, "\n"))
}

// pruneExpired bounds the two Postgres tables whose rows naturally expire.
func pruneExpired(ctx context.Context, st *store.Store, days int, log *slog.Logger) {
	tick := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	for {
		if n, err := st.PruneAudit(ctx, days); err != nil {
			log.Error("audit prune failed", "err", err)
		} else if n > 0 {
			log.Info("audit rows pruned", "rows", n, "retention_days", days)
		}
		if n, err := st.PruneSessions(ctx); err != nil {
			log.Error("session prune failed", "err", err)
		} else if n > 0 {
			log.Info("expired sessions pruned", "sessions", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
