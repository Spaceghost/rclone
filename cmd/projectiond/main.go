package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	httpadapter "github.com/Spaceghost/rclone-projection-vfs/internal/adapter/http"
	"github.com/Spaceghost/rclone-projection-vfs/internal/manifest"
	"github.com/Spaceghost/rclone-projection-vfs/internal/resolver"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	manifestPath := flag.String("manifest", "projection.cue", "path to a standalone CUE manifest")
	listenAddress := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	flag.Parse()

	m, err := manifest.LoadFile(*manifestPath)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	staticResolver, err := resolver.NewStatic(m)
	if err != nil {
		return fmt.Errorf("create resolver: %w", err)
	}

	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           httpadapter.NewHandler(staticResolver),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	slog.Info("projection resolver listening", "address", server.Addr, "manifest", *manifestPath)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	return nil
}
