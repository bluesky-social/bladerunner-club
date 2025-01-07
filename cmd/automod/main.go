package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/identity/redisdir"
	"github.com/bluesky-social/indigo/automod/consumer"

	"github.com/carlmjohnson/versioninfo"
	_ "github.com/joho/godotenv/autoload"
	cli "github.com/urfave/cli/v2"
	"golang.org/x/time/rate"
)

func main() {
	if err := run(os.Args); err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(-1)
	}
}

func run(args []string) error {

	app := cli.App{
		Name:    "automod",
		Usage:   "automod daemon (cleans the atmosphere)",
		Version: versioninfo.Short(),
	}

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "atp-relay-host",
			Usage:   "hostname and port of Relay to subscribe to",
			Value:   "wss://bsky.network",
			EnvVars: []string{"ATP_RELAY_HOST", "ATP_BGS_HOST"},
		},
		&cli.StringFlag{
			Name:    "atp-plc-host",
			Usage:   "method, hostname, and port of PLC registry",
			Value:   "https://plc.directory",
			EnvVars: []string{"ATP_PLC_HOST"},
		},
		&cli.StringFlag{
			Name:    "atp-bsky-host",
			Usage:   "method, hostname, and port of bsky API (appview) service. does not use auth",
			Value:   "https://public.api.bsky.app",
			EnvVars: []string{"ATP_BSKY_HOST"},
		},
		&cli.StringFlag{
			Name:    "atp-ozone-host",
			Usage:   "method, hostname, and port of ozone instance. requires ozone-admin-token as well",
			Value:   "https://mod.bsky.app",
			EnvVars: []string{"ATP_OZONE_HOST", "ATP_MOD_HOST"},
		},
		&cli.StringFlag{
			Name:    "ozone-did",
			Usage:   "DID of account to attribute ozone actions to",
			EnvVars: []string{"AUTOMOD_OZONE_DID"},
		},
		&cli.StringFlag{
			Name:    "ozone-admin-token",
			Usage:   "admin authentication password for mod service",
			EnvVars: []string{"AUTOMOD_OZONE_AUTH_ADMIN_TOKEN", "AUTOMOD_MOD_AUTH_ADMIN_TOKEN"},
		},
		&cli.StringFlag{
			Name:  "redis-url",
			Usage: "redis connection URL",
			// redis://<user>:<pass>@localhost:6379/<db>
			// redis://localhost:6379/0
			EnvVars: []string{"AUTOMOD_REDIS_URL"},
		},
		&cli.IntFlag{
			Name:    "plc-rate-limit",
			Usage:   "max number of requests per second to PLC registry",
			Value:   100,
			EnvVars: []string{"AUTOMOD_PLC_RATE_LIMIT"},
		},
		&cli.StringFlag{
			Name:    "log-level",
			Usage:   "log verbosity level (eg: warn, info, debug)",
			EnvVars: []string{"AUTOMOD_LOG_LEVEL", "LOG_LEVEL"},
		},
		&cli.IntFlag{
			Name:    "firehose-parallelism",
			Usage:   "force a fixed number of parallel firehose workers. default (or 0) for auto-scaling; 200 works for a large instance",
			Value:   200,
			EnvVars: []string{"AUTOMOD_FIREHOSE_PARALLELISM"},
		},
	}

	app.Commands = []*cli.Command{
		runCmd,
	}

	return app.Run(args)
}

func configDirectory(cctx *cli.Context) (identity.Directory, error) {
	baseDir := identity.BaseDirectory{
		PLCURL: cctx.String("atp-plc-host"),
		HTTPClient: http.Client{
			Timeout: time.Second * 15,
		},
		PLCLimiter:            rate.NewLimiter(rate.Limit(cctx.Int("plc-rate-limit")), 1),
		TryAuthoritativeDNS:   true,
		SkipDNSDomainSuffixes: []string{".bsky.social", ".staging.bsky.dev"},
	}
	var dir identity.Directory
	if cctx.String("redis-url") != "" {
		rdir, err := redisdir.NewRedisDirectory(&baseDir, cctx.String("redis-url"), time.Hour*24, time.Minute*2, time.Minute*5, 10_000)
		if err != nil {
			return nil, err
		}
		dir = rdir
	} else {
		cdir := identity.NewCacheDirectory(&baseDir, 1_500_000, time.Hour*24, time.Minute*2, time.Minute*5)
		dir = &cdir
	}
	return dir, nil
}

func configLogger(cctx *cli.Context, writer io.Writer) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cctx.String("log-level")) {
	case "error":
		level = slog.LevelError
	case "warn":
		level = slog.LevelWarn
	case "info":
		level = slog.LevelInfo
	case "debug":
		level = slog.LevelDebug
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)
	return logger
}

var runCmd = &cli.Command{
	Name:  "run",
	Usage: "run the hepa daemon",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "metrics-listen",
			Usage:   "IP or address, and port, to listen on for metrics APIs",
			Value:   ":3989",
			EnvVars: []string{"AUTOMOD_METRICS_LISTEN"},
		},
		&cli.StringFlag{
			Name: "slack-webhook-url",
			// eg: https://hooks.slack.com/services/X1234
			Usage:   "full URL of slack webhook",
			EnvVars: []string{"SLACK_WEBHOOK_URL"},
		},
	},
	Action: func(cctx *cli.Context) error {
		ctx := context.Background()
		logger := configLogger(cctx, os.Stdout)

		dir, err := configDirectory(cctx)
		if err != nil {
			return fmt.Errorf("failed to configure identity directory: %v", err)
		}

		srv, err := NewServer(
			dir,
			Config{
				Logger:          logger,
				BskyHost:        cctx.String("atp-bsky-host"),
				OzoneHost:       cctx.String("atp-ozone-host"),
				OzoneDID:        cctx.String("ozone-did"),
				OzoneAdminToken: cctx.String("ozone-admin-token"),
				RedisURL:        cctx.String("redis-url"),
				RatelimitBypass: cctx.String("ratelimit-bypass"),
			},
		)
		if err != nil {
			return fmt.Errorf("failed to construct server: %v", err)
		}

		// ozone event consumer (if configured)
		if srv.Engine.OzoneClient != nil {
			oc := consumer.OzoneConsumer{
				Logger:      logger.With("subsystem", "ozone-consumer"),
				RedisClient: srv.RedisClient,
				OzoneClient: srv.Engine.OzoneClient,
				Engine:      srv.Engine,
			}

			go func() {
				if err := oc.Run(ctx); err != nil {
					slog.Error("ozone consumer failed", "err", err)
				}
			}()

			go func() {
				if err := oc.RunPersistCursor(ctx); err != nil {
					slog.Error("ozone cursor routine failed", "err", err)
				}
			}()
		}

		// prometheus HTTP endpoint: /metrics
		go func() {
			runtime.SetBlockProfileRate(10)
			runtime.SetMutexProfileFraction(10)
			if err := srv.RunMetrics(cctx.String("metrics-listen")); err != nil {
				slog.Error("failed to start metrics endpoint", "error", err)
				panic(fmt.Errorf("failed to start metrics endpoint: %w", err))
			}
		}()

		// firehose event consumer (note this is actually mandatory)
		relayHost := cctx.String("atp-relay-host")
		if relayHost != "" {
			fc := consumer.FirehoseConsumer{
				Engine:      srv.Engine,
				Logger:      logger.With("subsystem", "firehose-consumer"),
				Host:        cctx.String("atp-relay-host"),
				Parallelism: cctx.Int("firehose-parallelism"),
				RedisClient: srv.RedisClient,
			}

			go func() {
				if err := fc.RunPersistCursor(ctx); err != nil {
					slog.Error("cursor routine failed", "err", err)
				}
			}()

			if err := fc.Run(ctx); err != nil {
				return fmt.Errorf("failure consuming and processing firehose: %w", err)
			}
		}

		return nil
	},
}
