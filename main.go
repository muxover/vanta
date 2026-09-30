package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/muxover/vanta/internal/config"
	"github.com/muxover/vanta/internal/metrics"
	"github.com/muxover/vanta/internal/proxy"
	"github.com/muxover/vanta/internal/sys"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const version = "1.4.0"

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to the config file")
	check := flag.Bool("check", false, "validate the config and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *check {
		fmt.Println("config ok")
		return
	}

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	setLogLevel(cfg)

	if err := run(*cfgPath, cfg); err != nil {
		log.Fatal().Err(err).Msg("vanta stopped")
	}
}

func run(cfgPath string, cfg *config.Config) error {
	if err := sys.EnableNonlocalBind(); err != nil {
		log.Warn().Err(err).Msg("ip_nonlocal_bind not set; binding relies on the local routes")
	}
	if cfg.TuneKernel {
		for _, err := range sys.Tune() {
			log.Warn().Err(err).Msg("kernel tuning skipped")
		}
	}
	routes := sys.NewRoutes()
	defer func() {
		if err := routes.Close(); err != nil {
			log.Warn().Err(err).Msg("route cleanup")
		}
	}()
	syncRoutes(routes, cfg)
	warnIfOpen(cfg)

	srv, err := proxy.New(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.MetricsPort > 0 {
		go serveMetrics(ctx, cfg.MetricsAddr())
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			next, err := config.Load(cfgPath)
			if err != nil {
				log.Error().Err(err).Msg("reload failed, keeping the running config")
				continue
			}
			if next.ListenAddr() != cfg.ListenAddr() || next.MetricsAddr() != cfg.MetricsAddr() || next.MetricsPort != cfg.MetricsPort {
				log.Warn().Msg("listen and metrics addresses only change on restart")
			}
			if err := srv.Reload(next); err != nil {
				log.Error().Err(err).Msg("reload failed, keeping the running config")
				continue
			}
			syncRoutes(routes, next)
			setLogLevel(next)
			warnIfOpen(next)
			log.Info().Int("prefixes", len(next.Prefixes)).Msg("config reloaded")
		}
	}()

	return srv.Run(ctx)
}

func syncRoutes(r *sys.Routes, cfg *config.Config) {
	var prefixes []netip.Prefix
	if cfg.AddRoutes {
		prefixes = cfg.Prefixes
	}
	if err := r.Sync(prefixes); err != nil {
		log.Error().Err(err).Msg("routes")
	}
}

func warnIfOpen(cfg *config.Config) {
	if cfg.AuthEnabled() || len(cfg.Allow) > 0 {
		return
	}
	if a, err := netip.ParseAddr(cfg.ListenAddress); err == nil && a.IsLoopback() {
		return
	}
	log.Warn().Msg("no auth and no allow_ips: anyone who can reach this port can use the proxy")
}

func setLogLevel(cfg *config.Config) {
	if cfg.Debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	} else {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}

func serveMetrics(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metrics.Handler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Info().Str("addr", addr).Msg("metrics listening")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error().Err(err).Msg("metrics server stopped")
	}
}
