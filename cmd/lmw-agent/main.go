// Command lmw-agent runs the Local Model Works node agent: it enrolls with
// the controller, keeps the control session alive, runs managed workload
// containers, and listens for peer artifact transfers.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jj-link/local-model-works/internal/agent"
	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/runtime"
)

// Version and Commit are stamped at build time.
var (
	Version = "dev"
	Commit  = "none"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		if err := run(); err != nil {
			log.Fatalf("lmw-agent: %v", err)
		}
	case "install":
		install()
	case "version", "--version":
		fmt.Printf("lmw-agent %s (%s)\n", Version, Commit)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: lmw-agent <run|install|version>")
	os.Exit(2)
}

func run() error {
	cfg, err := config.LoadAgent()
	if err != nil {
		return err
	}
	rt, err := runtime.NewDocker(cfg.DockerSocket)
	if err != nil {
		return fmt.Errorf("docker runtime: %w", err)
	}
	a := agent.New(cfg, Version, Commit, rt, nil)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}

// install prepares the node state layout and prints the environment the
// operator must provide. It is idempotent and never touches running state.
func install() {
	cfg, err := config.LoadAgent()
	if err != nil {
		log.Fatalf("lmw-agent install: %v", err)
	}
	dirs := []string{
		cfg.StateRoot,
		cfg.CADir(),
		cfg.TransferDir(),
		cfg.LogDir(),
		cfg.Workspace,
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("lmw-agent install: %v", err)
		}
	}
	fmt.Println("agent state prepared:")
	for _, d := range dirs {
		fmt.Printf("  %s\n", d)
	}
	fmt.Println("\nrequired environment for `lmw-agent run`:")
	fmt.Printf("  %s=<controller mTLS base URL>\n", config.EnvAgentServer)
	fmt.Printf("  %s=<SHA-256 hex of the controller CA>\n", config.EnvAgentCASha256)
	fmt.Printf("  %s=<one-time enrollment token>\n", config.EnvAgentToken)
	fmt.Printf("  %s=<peer transfer listen address>\n", config.EnvPeerAddr)
	fmt.Printf("  %s=<optional docker socket>\n", config.EnvAgentDockerSock)
	fmt.Printf("  %s=<optional colon-separated cache roots>\n", config.EnvAgentCacheRoots)
}
