package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/denverquane/stilton/internal/db"
	"github.com/denverquane/stilton/internal/library"
	"github.com/denverquane/stilton/internal/server"
	"github.com/denverquane/stilton/internal/thumbnail"
	"github.com/denverquane/stilton/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr     = flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
		libDir   = flag.String("library", "", "directory of model files to index (required)")
		dataDir  = flag.String("data", "data", "directory for the database and thumbnails")
		workers  = flag.Int("workers", 2, "concurrent thumbnail renders (each large mesh can use hundreds of MB)")
		interval = flag.Duration("rescan", 5*time.Minute, "how often to rescan the library (0 disables)")
	)
	flag.Parse()
	if *libDir == "" {
		flag.Usage()
		return errors.New("-library is required")
	}
	root, err := filepath.Abs(*libDir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("library %q is not a directory", root)
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return err
	}

	database, err := db.Open(*dataDir)
	if err != nil {
		return err
	}
	defer database.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	thumbs := thumbnail.NewGenerator(database, root, filepath.Join(*dataDir, "thumbs"), *workers)
	scanner := &library.Scanner{Root: root, DB: database, OnChange: thumbs.Wake}
	// Keep thumbnails for indexed models and for models in the trash, so a
	// restore doesn't have to re-render them.
	sweep := func() {
		n, err := thumbs.Sweep(ctx, func(ctx context.Context) (map[string]bool, error) {
			keep, err := database.AllHashes(ctx)
			if err != nil {
				return nil, err
			}
			trashed, err := scanner.TrashHashes()
			if err != nil {
				return nil, err
			}
			for h := range trashed {
				keep[h] = true
			}
			return keep, nil
		})
		if err != nil {
			slog.Error("thumbnail sweep failed", "err", err)
		} else if n > 0 {
			slog.Info("swept unused thumbnails", "removed", n)
		}
	}
	scanner.OnRemoved = sweep

	go func() {
		if err := thumbs.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("thumbnail generator stopped", "err", err)
		}
	}()
	// Sweep after the first scan, not before: on a fresh database every
	// thumbnail would look unused.
	go scanLoop(ctx, scanner, *interval, sweep)

	srv := &http.Server{
		Addr:    *addr,
		Handler: (&server.Server{DB: database, Scanner: scanner, Thumbs: thumbs, Web: web.Dist()}).Handler(),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	slog.Info("stilton listening", "addr", "http://"+*addr, "library", root)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func scanLoop(ctx context.Context, s *library.Scanner, interval time.Duration, afterFirst func()) {
	scan := func() {
		start := time.Now()
		res, err := s.Scan(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("scan failed", "err", err)
			}
			return
		}
		slog.Info("scan complete", "added", res.Added, "updated", res.Updated, "removed", res.Removed,
			"took", time.Since(start).Round(time.Millisecond))
	}

	scan()
	if ctx.Err() == nil {
		afterFirst()
	}
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scan()
		}
	}
}
