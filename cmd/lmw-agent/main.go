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
		if err := install(os.Args[2:], os.Stdout); err != nil {
			log.Fatalf("lmw-agent install: %v", err)
		}
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
