package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/bluesky-social/indigo/automod"
	"github.com/bluesky-social/indigo/automod/cachestore"
	"github.com/bluesky-social/indigo/automod/countstore"
	"github.com/bluesky-social/indigo/automod/flagstore"
	"github.com/bluesky-social/indigo/util"
	"github.com/bluesky-social/indigo/xrpc"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	Engine      *automod.Engine
	RedisClient *redis.Client

	logger *slog.Logger
}

type Config struct {
	Logger          *slog.Logger
	BskyHost        string
	OzoneHost       string
	OzoneDID        string
	OzoneAdminToken string
	RedisURL        string
	RulesetName     string
	RatelimitBypass string
	SlackWebhookURL string
}

func NewServer(dir identity.Directory, config Config) (*Server, error) {
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
	}

	var ozoneClient *xrpc.Client
	if config.OzoneAdminToken != "" && config.OzoneDID != "" {
		ozoneClient = &xrpc.Client{
			Client:     util.RobustHTTPClient(),
			Host:       config.OzoneHost,
			AdminToken: &config.OzoneAdminToken,
			Auth:       &xrpc.AuthInfo{},
		}
		if config.RatelimitBypass != "" {
			ozoneClient.Headers = make(map[string]string)
			ozoneClient.Headers["x-ratelimit-bypass"] = config.RatelimitBypass
		}
		od, err := syntax.ParseDID(config.OzoneDID)
		if err != nil {
			return nil, fmt.Errorf("ozone account DID supplied was not valid: %v", err)
		}
		ozoneClient.Auth.Did = od.String()
		logger.Info("configured ozone admin client", "did", od.String(), "ozoneHost", config.OzoneHost)
	} else {
		logger.Info("did not configure ozone client")
	}

	var counters countstore.CountStore
	var cache cachestore.CacheStore
	var flags flagstore.FlagStore
	var rdb *redis.Client
	if config.RedisURL != "" {
		// generic client, for cursor state
		opt, err := redis.ParseURL(config.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("parsing redis URL: %v", err)
		}
		rdb = redis.NewClient(opt)
		// check redis connection
		_, err = rdb.Ping(context.TODO()).Result()
		if err != nil {
			return nil, fmt.Errorf("redis ping failed: %v", err)
		}

		cnt, err := countstore.NewRedisCountStore(config.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("initializing redis countstore: %v", err)
		}
		counters = cnt

		csh, err := cachestore.NewRedisCacheStore(config.RedisURL, 6*time.Hour)
		if err != nil {
			return nil, fmt.Errorf("initializing redis cachestore: %v", err)
		}
		cache = csh

		flg, err := flagstore.NewRedisFlagStore(config.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("initializing redis flagstore: %v", err)
		}
		flags = flg
	} else {
		counters = countstore.NewMemCountStore()
		cache = cachestore.NewMemCacheStore(5_000, 1*time.Hour)
		flags = flagstore.NewMemFlagStore()
	}

	ruleset := automod.RuleSet{
		PostRules: []automod.PostRuleFunc{
			GoodBotBadBotRule,
		},
	}

	var notifier automod.Notifier
	if config.SlackWebhookURL != "" {
		notifier = &automod.SlackNotifier{
			SlackWebhookURL: config.SlackWebhookURL,
		}
	}

	bskyClient := xrpc.Client{
		Client: util.RobustHTTPClient(),
		Host:   config.BskyHost,
	}
	if config.RatelimitBypass != "" {
		bskyClient.Headers = make(map[string]string)
		bskyClient.Headers["x-ratelimit-bypass"] = config.RatelimitBypass
	}
	eng := automod.Engine{
		Logger:    logger,
		Directory: dir,
		Counters:  counters,
		//Sets:        sets,
		Flags:    flags,
		Cache:    cache,
		Rules:    ruleset,
		Notifier: notifier,
		// TODO: rip out more bskyClient stuff
		//BskyClient:  &bskyClient,
		OzoneClient: ozoneClient,
		//AdminClient: adminClient,
		//BlobClient:  blobClient,
		Config: automod.EngineConfig{
			SkipAccountMeta: true,
		},
	}

	s := &Server{
		logger:      logger,
		Engine:      &eng,
		RedisClient: rdb,
	}

	return s, nil
}

func (s *Server) RunMetrics(listen string) error {
	http.Handle("/metrics", promhttp.Handler())
	return http.ListenAndServe(listen, nil)
}
