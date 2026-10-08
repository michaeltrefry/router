// Command personalkey issues a personal router key on a self-hosted router's
// admin installation, so its operator can enroll server-side Claude and Codex
// subscriptions (`install.sh login claude|codex`). Installation-shared keys
// cannot own subscriptions; a personal key is bound to a credential subject.
//
// Usage:
//
//	go run ./cmd/personalkey -email you@example.com [-rotate]
//	# or via Makefile:
//	make personal-key EMAIL=you@example.com [ROTATE=1]
//
// The raw key is printed once to stdout and never stored or logged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/config"
	"weave-os/router/internal/postgres"
	"weave-os/router/internal/postgres/pgtls"
	"weave-os/router/internal/server"
)

const baseURL = "http://localhost:8080"

type issuer interface {
	IssueSelfHostedPersonalKey(context.Context, auth.IssuePersonalKeyParams) (*auth.PersonalKeyIssue, error)
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr, connect))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, open func(context.Context) (issuer, func(), error)) int {
	flags := flag.NewFlagSet("personalkey", flag.ContinueOnError)
	flags.SetOutput(stderr)
	email := flags.String("email", "", "operator email the personal key belongs to (required)")
	rotate := flags.Bool("rotate", false, "replace the existing key for this email, keeping its enrolled subscriptions")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *email == "" {
		fmt.Fprintln(stderr, "Error: -email is required (make personal-key EMAIL=you@example.com)")
		return 2
	}
	if mode := getenv("ROUTER_DEPLOYMENT_MODE"); mode != "" && server.DeploymentMode(mode) != server.DeploymentModeSelfHosted {
		fmt.Fprintf(stderr, "Error: personal keys can only be issued here on a self-hosted router (ROUTER_DEPLOYMENT_MODE=%q); managed deployments issue them from the control plane.\n", mode)
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	svc, closeDB, err := open(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "Error: cannot connect to postgres: %v\n", err)
		return 1
	}
	defer closeDB()

	issued, err := svc.IssueSelfHostedPersonalKey(ctx, auth.IssuePersonalKeyParams{Email: *email, Rotate: *rotate})
	switch {
	case errors.Is(err, auth.ErrPersonalKeyExists):
		fmt.Fprintf(stderr, "Error: %s already has a personal key. Re-run with -rotate (make personal-key EMAIL=%s ROTATE=1) to replace it; enrolled subscriptions are kept.\n", *email, *email)
		return 1
	case errors.Is(err, auth.ErrAdminInstallationMissing):
		fmt.Fprintln(stderr, "Error: this database has no self-hosted admin installation. Run `make seed` (or sign in to the dashboard once) first.")
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "Error: cannot issue personal key: %v\n", err)
		return 1
	}

	verb := "Issued"
	if issued.Rotated {
		verb = "Rotated"
	}
	fmt.Fprintf(stdout, "%s personal router key %s (suffix: ...%s) on installation %s\n", verb, issued.Key.ID, issued.Key.KeySuffix, issued.Installation.ID)
	fmt.Fprintf(stdout, "Credential subject %s\n\n", issued.Key.CredentialSubjectID)
	fmt.Fprintf(stdout, "Personal router key (shown once — store it now):\n  %s\n\n", issued.RawToken)
	if issued.Rotated {
		fmt.Fprintln(stdout, "The replaced key is revoked; a running router may accept it until its auth cache expires (up to 5 minutes).")
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "Check it:\n  curl -i -H 'X-Weave-Router-Key: %s' %s/validate\n\n", issued.RawToken, baseURL)
	fmt.Fprintln(stdout, "Enroll subscriptions (router needs ROUTER_SUBSCRIPTION_POOLS_ENABLED=true and EXTERNAL_KEY_ENCRYPTION_KEY):")
	fmt.Fprintf(stdout, "  WEAVE_ROUTER_KEY=%s ./install/install.sh login claude --local\n", issued.RawToken)
	fmt.Fprintf(stdout, "  WEAVE_ROUTER_KEY=%s ./install/install.sh login codex --local\n", issued.RawToken)
	return 0
}

func connect(ctx context.Context) (issuer, func(), error) {
	cfg, err := pgxpool.ParseConfig(config.PostgresDSN())
	if err != nil {
		return nil, nil, err
	}
	if _, err := pgtls.Configure(cfg); err != nil {
		return nil, nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO router, public")
		return err
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	repo := postgres.NewRepository(pool, auth.NoOpEncryptor{})
	svc := auth.NewService(repo.Installations, repo.APIKeys, repo.ExternalAPIKeys, repo.Users, auth.NoOpAPIKeyCache{}, nil, time.Now).
		WithPersonalKeyStore(postgres.NewCredentialSubjectRepo(pool))
	return svc, pool.Close, nil
}
