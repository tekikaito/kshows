// kshows — a spatial map of Kubernetes node capacity and how workloads fill it.
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
	"syscall"
	"time"

	"github.com/tekikaito/kshows/internal/collector"
	"github.com/tekikaito/kshows/internal/kube"
	"github.com/tekikaito/kshows/internal/metrics"
	"github.com/tekikaito/kshows/internal/server"
	"github.com/tekikaito/kshows/web"
)

// version is stamped at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	listen := flag.String("listen", ":8080", "address to serve HTTP on")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (local mode; defaults to standard loading rules)")
	pollInterval := flag.Duration("poll-interval", 15*time.Second, "how often to refresh usage from the Metrics Server")
	mock := flag.Bool("mock", false, "serve simulated cluster data (no cluster needed; for demos and UI development)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("kshows", version)
		return
	}

	metrics.SetVersion(version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var source collector.Source
	var run func(context.Context) error

	if *mock {
		log.Println("running in mock mode: serving simulated cluster data")
		m := collector.NewMock(time.Now().UnixNano())
		source, run = m, m.Run
	} else {
		clients, err := kube.New(*kubeconfig)
		if err != nil {
			log.Fatalf("connecting to cluster: %v", err)
		}
		c := collector.New(clients, *pollInterval)
		source, run = c, c.Run
	}

	go func() {
		if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatalf("collector stopped: %v", err)
		}
	}()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           server.New(source, web.Static()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("kshows %s serving on %s", version, *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}
}
