// clusterd is the platform API. It runs on every hypervisor; Keepalived
// moves the API address to an instance whose /v1/health answers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"github.com/jurrebuunk/clusterd/internal/actions"
	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/cmdrun"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kube"
	"github.com/jurrebuunk/clusterd/internal/server"
	"github.com/jurrebuunk/clusterd/internal/status"
	"github.com/jurrebuunk/clusterd/internal/version"
)

func main() {
	cfgPath := flag.String("config", "/etc/clusterd/config.toml", "configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	checkOnly := flag.Bool("check", false, "load the configuration and token file, then exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("clusterd", version.String())
		return
	}

	// The journal adds timestamps.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	if err := run(*cfgPath, *checkOnly, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, checkOnly bool, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tokens, err := auth.Load(cfg.TokensFile)
	if err != nil {
		return fmt.Errorf("tokens: %w", err)
	}
	var kc *kube.Client
	if cfg.Kubernetes.Kubeconfig != "" {
		if kc, err = kube.FromKubeconfig(cfg.Kubernetes.Kubeconfig); err != nil {
			return fmt.Errorf("kubernetes: %w", err)
		}
	}
	var certs *server.CertReloader
	if cfg.TLS.Cert != "" {
		if certs, err = server.NewCertReloader(cfg.TLS.Cert, cfg.TLS.Key); err != nil {
			return fmt.Errorf("tls: %w", err)
		}
	}
	if checkOnly {
		fmt.Printf("ok: site %s, host %s, %d tokens\n", cfg.Site, cfg.Host, tokens.Len())
		return nil
	}

	var eng *actions.Engine
	if kc != nil {
		eng = &actions.Engine{Cfg: cfg, Kube: kc, Run: cmdrun.Run, Log: log, Timing: actions.DefaultTiming}
	}
	collector := status.New(cfg, kc, log)
	srv, err := server.New(cfg, tokens, collector, eng, log)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	if certs != nil {
		httpSrv.TLSConfig = certs.TLSConfig()
	} else {
		log.Warn("no TLS configured: serving plain HTTP (development only)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go collector.Run(ctx)
	errc := make(chan error, len(cfg.Listen)+1)

	for _, addr := range cfg.Listen {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		log.Info("listening", "addr", ln.Addr().String(), "tls", certs != nil)
		go func() {
			if certs != nil {
				errc <- httpSrv.ServeTLS(ln, "", "")
			} else {
				errc <- httpSrv.Serve(ln)
			}
		}()
	}

	var metricsSrv *http.Server
	if cfg.MetricsListen != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
			metrics.WritePrometheus(w, true)
		})
		metricsSrv = &http.Server{Addr: cfg.MetricsListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { errc <- metricsSrv.ListenAndServe() }()
	}
	metrics.GetOrCreateGauge(`clusterd_tokens`, func() float64 { return float64(tokens.Len()) })
	metrics.GetOrCreateCounter(fmt.Sprintf(`clusterd_build_info{version=%q,commit=%q}`, version.Version, version.Commit)).Set(1)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	log.Info("started", "version", version.Version, "site", cfg.Site, "host", cfg.Host, "tokens", tokens.Len())

	for {
		select {
		case <-hup:
			// Reload what can change without a restart: tokens and the certificate.
			if err := tokens.Reload(); err != nil {
				log.Error("reload tokens failed, keeping the old ones", "err", err)
			} else {
				log.Info("reloaded tokens", "tokens", tokens.Len())
			}
			if certs != nil {
				if err := certs.Reload(); err != nil {
					log.Error("reload certificate failed, keeping the old one", "err", err)
				} else {
					log.Info("reloaded certificate")
				}
			}
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		case <-ctx.Done():
			log.Info("shutting down")
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if metricsSrv != nil {
				_ = metricsSrv.Shutdown(sctx)
			}
			return httpSrv.Shutdown(sctx)
		}
	}
}
