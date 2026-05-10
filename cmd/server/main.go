package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/api"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/cache"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/config"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/job"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/pipeline"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/worker"
)

func main() {
	batchMode := flag.Bool("batch", false, "categorize all uncategorized withdrawals and exit")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, store, err := config.Load()
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	reg := job.NewRegistry()
	webhookPool := worker.NewPool(cfg.WorkerConcurrency, 1000)
	batchPool := worker.NewPool(cfg.BatchConcurrency, 10000)

	webhookPool.Start()
	batchPool.Start()

	if *batchMode {
		runBatch(cfg, reg, batchPool)
		return
	}

	handler, err := api.New(cfg, store, reg, webhookPool, batchPool)
	if err != nil {
		slog.Error("handler init", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: handler.Router(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("server starting",
			"port", cfg.Port,
			"ui", cfg.EnableUI,
			"configured", cfg.IsConfigured(),
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = srv.Shutdown(shutdownCtx)
	webhookPool.Shutdown()
	batchPool.Shutdown()
}

func runBatch(cfg *config.Config, reg *job.Registry, pool *worker.Pool) {
	if !cfg.IsConfigured() {
		slog.Error("batch: not configured — set required environment variables")
		pool.Shutdown()
		os.Exit(1)
	}

	cl, err := classifier.Build(classifier.BuildParams{
		Provider:      cfg.AIProvider,
		OpenAIKey:     cfg.OpenAIKey,
		OpenAIModel:   cfg.OpenAIModel,
		OpenAIBaseURL: cfg.OpenAIBaseURL,
		GeminiKey:     cfg.GeminiKey,
		GeminiModel:   cfg.GeminiModel,
		DeepseekKey:   cfg.DeepseekKey,
		DeepseekModel: cfg.DeepseekModel,
		CustomContext: cfg.CustomSystemContext,
	})
	if err != nil {
		slog.Error("batch: classifier init failed", "error", err)
		pool.Shutdown()
		os.Exit(1)
	}

	fc := firefly.New(cfg.FireflyURL, cfg.FireflyToken, cfg.TagPrefix)
	ca := cache.New(fc, cfg.HistoryCacheTTL, cfg.HistoryLookbackDays)
	pipe := pipeline.New(fc, cl, ca, reg, cfg.HistoryContextLimit)

	ctx := context.Background()

	slog.Info("batch: fetching uncategorized withdrawals")
	txns, err := fc.GetUncategorizedWithdrawals(ctx)
	if err != nil {
		slog.Error("batch: fetch failed", "error", err)
		pool.Shutdown()
		os.Exit(1)
	}
	slog.Info("batch: enqueuing", "count", len(txns))

	for _, txn := range txns {
		if len(txn.Splits) == 0 {
			continue
		}
		first := txn.Splits[0]

		var amount *float64
		if v, err := strconv.ParseFloat(first.Amount, 64); err == nil {
			amount = &v
		}

		j := reg.Create(txn.ID, "cli-batch", first.DestinationName, first.Description, amount)
		txnID := txn.ID
		splits := txn.Splits

		pool.Submit(worker.Task{
			JobID: j.ID,
			Execute: func(ctx context.Context) error {
				return pipe.Run(ctx, j, txnID, splits)
			},
		})
	}

	pool.Shutdown()

	finished, failed := 0, 0
	for _, j := range reg.List() {
		switch j.Status {
		case job.StatusFinished:
			finished++
		case job.StatusFailed:
			failed++
		}
	}
	slog.Info("batch complete", "finished", finished, "failed", failed, "total", len(txns))
}
