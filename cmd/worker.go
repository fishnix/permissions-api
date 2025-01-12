package cmd

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.infratographer.com/x/echox"
	"go.infratographer.com/x/events"
	"go.infratographer.com/x/otelx"
	"go.infratographer.com/x/versionx"
	"go.infratographer.com/x/viperx"
	"go.uber.org/zap"

	"go.infratographer.com/permissions-api/internal/config"
	"go.infratographer.com/permissions-api/internal/iapl"
	"go.infratographer.com/permissions-api/internal/pubsub"
	"go.infratographer.com/permissions-api/internal/query"
	"go.infratographer.com/permissions-api/internal/spicedbx"
)

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "starts a permissions-api queue worker",
	Run: func(cmd *cobra.Command, args []string) {
		worker(cmd.Context(), globalCfg)
	},
}

func init() {
	rootCmd.AddCommand(workerCmd)

	otelx.MustViperFlags(viper.GetViper(), workerCmd.Flags())
	events.MustViperFlagsForSubscriber(viper.GetViper(), workerCmd.Flags())
	echox.MustViperFlags(viper.GetViper(), workerCmd.Flags(), apiDefaultListen)

	workerCmd.PersistentFlags().StringSlice("events-topics", []string{}, "event topics to subscribe to")
	viperx.MustBindFlag(viper.GetViper(), "events.topics", workerCmd.PersistentFlags().Lookup("events-topics"))
}

func worker(ctx context.Context, cfg *config.AppConfig) {
	err := otelx.InitTracer(cfg.Tracing, appName, logger)
	if err != nil {
		logger.Fatalw("unable to initialize tracing system", "error", err)
	}

	spiceClient, err := spicedbx.NewClient(cfg.SpiceDB, cfg.Tracing.Enabled)
	if err != nil {
		logger.Fatalw("unable to initialize spicedb client", "error", err)
	}

	var policy iapl.Policy

	if cfg.SpiceDB.PolicyFile != "" {
		policy, err = iapl.NewPolicyFromFile(cfg.SpiceDB.PolicyFile)
		if err != nil {
			logger.Fatalw("unable to load new policy from schema file", "policy_file", cfg.SpiceDB.PolicyFile, "error", err)
		}
	} else {
		logger.Warn("no spicedb policy file defined, using default policy")

		policy = iapl.DefaultPolicy()
	}

	if err = policy.Validate(); err != nil {
		logger.Fatalw("invalid spicedb policy", "error", err)
	}

	engine := query.NewEngine("infratographer", spiceClient, query.WithPolicy(policy), query.WithLogger(logger))

	subscriber, err := pubsub.NewSubscriber(ctx, cfg.Events.Subscriber, engine, pubsub.WithLogger(logger))
	if err != nil {
		logger.Fatalw("unable to initialize subscriber", "error", err)
	}

	defer subscriber.Close()

	for _, topic := range viper.GetStringSlice("events.topics") {
		if err := subscriber.Subscribe(topic); err != nil {
			logger.Fatalw("failed to subscribe to changes topic", "topic", topic, "error", err)
		}
	}

	logger.Info("Listening for events")

	go func() {
		if err := subscriber.Listen(); err != nil {
			logger.Fatalw("error listening for events", "error", err)
		}
	}()

	srv, err := echox.NewServer(
		logger.Desugar(),
		echox.ConfigFromViper(viper.GetViper()),
		versionx.BuildDetails(),
	)
	if err != nil {
		logger.Fatal("failed to initialize new server", zap.Error(err))
	}

	srv.AddReadinessCheck("spicedb", spicedbx.Healthcheck(spiceClient))

	if err := srv.Run(); err != nil {
		logger.Fatal("failed to run server", zap.Error(err))
	}
}
