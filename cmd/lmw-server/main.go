// Command lmw-server runs the Local Model Works control plane: the browser
// and CLI HTTP API plus the agent mTLS listener.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/server"
	"github.com/jj-link/local-model-works/internal/sign"
)

// Version and commit are stamped at build time.
var (
	Version = "dev"
	Commit  = "none"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(config.LoadServer(), Version, Commit); err != nil {
		log.Fatalf("lmw-server: %v", err)
	}
}

func run(cfg config.Server, version, commit string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, d := range []string{cfg.StateRoot, filepath.Join(cfg.StateRoot, "ca"), cfg.RecipeRoot(), cfg.RunRoot(), cfg.CatalogRoot()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("state root: %w", err)
		}
	}

	// Materialize the recipe/catalog trust key once: an operator-supplied PEM
	// from the environment wins; otherwise generate a fresh Ed25519 key on
	// first boot so the server never fails to start on an empty state dir.
	if cfg.TrustKeyPEM != "" {
		if err := os.WriteFile(cfg.TrustKeyPath(), []byte(cfg.TrustKeyPEM+"\n"), 0o600); err != nil {
			return fmt.Errorf("trust key: %w", err)
		}
	} else if _, err := os.Stat(cfg.TrustKeyPath()); os.IsNotExist(err) {
		keyPEM, genErr := sign.NewKeyPEM()
		if genErr != nil {
			return fmt.Errorf("trust key: %w", genErr)
		}
		if err := os.WriteFile(cfg.TrustKeyPath(), keyPEM, 0o600); err != nil {
			return fmt.Errorf("trust key: %w", err)
		}
	}

	sqlDB, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer sqlDB.Close()
	q := db.New(sqlDB)

	// The operator is created explicitly before first start. Refuse to expose
	// listeners without credentials rather than accepting a reusable secret
	// from the service environment.
	n, err := q.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no operator user exists; run `lmw admin create --state %s` before starting lmw-server", cfg.StateRoot)
	}
	if _, err := cfg.NormalizedPublicOrigin(); err != nil {
		return err
	}
	if _, err := cfg.NormalizedPublicAgentURL(); err != nil {
		return err
	}

	chain, err := ca.LoadKeyCert(cfg.CAKeyPath(), cfg.CACertPath())
	if err != nil {
		return fmt.Errorf("load CA: %w", err)
	}
	sessions := &auth.Sessions{
		TTL: cfg.SessionTTL,
		Create: func(tokenHash, username, csrfHash, expiresAt string) error {
			return q.CreateSession(ctx, db.CreateSessionParams{
				TokenHash: tokenHash, Username: username, CsrfHash: csrfHash,
				CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), ExpiresAt: expiresAt,
			})
		},
		Get: func(tokenHash string) (string, string, string, error) {
			row, err := q.GetSessionByTokenHash(ctx, tokenHash)
			if err != nil {
				return "", "", "", err
			}
			return row.Username, row.CsrfHash, row.ExpiresAt, nil
		},
		Delete: func(tokenHash string) error {
			return q.DeleteSessionByTokenHash(ctx, tokenHash)
		},
	}

	srv := server.New(server.Deps{
		Ctx:      ctx,
		Cfg:      cfg,
		DB:       sqlDB,
		Q:        q,
		CA:       chain,
		Sessions: sessions,
		Version:  version,
		Commit:   commit,
	})

	if n, err := srv.Recover(ctx); err != nil {
		return fmt.Errorf("recover interrupted runs: %w", err)
	} else if n > 0 {
		log.Printf("recovered: %d interrupted one-shot runs marked", n)
	}
	agentAddr, err := srv.StartAgentListener(ctx)
	if err != nil {
		return fmt.Errorf("agent listener: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("http listener: %w", err)
	}
	httpSrv := &http.Server{Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}()

	log.Printf("lmw-server %s (%s)", version, commit)
	log.Printf("http  %s", ln.Addr())
	log.Printf("agent %s (mTLS; CA fingerprint %s)", agentAddr, chain.Fingerprint())
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
