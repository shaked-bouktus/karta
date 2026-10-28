// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

// Command karta-metrics-exporter starts the exporter scaffold.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/internal/config"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/submitterextractor"
	"github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Karta metrics exporter failed", "error", err)
		os.Exit(1)
	}
}

// run keeps the signal handler's deferred cleanup out of main, since os.Exit skips defers.
func run() error {
	fs := flag.NewFlagSet("karta-metrics-exporter", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "Print the exporter version and exit.")
	loader := config.NewLoader(fs, os.Getenv)
	switch err := fs.Parse(os.Args[1:]); {
	case errors.Is(err, flag.ErrHelp):
		return nil
	case err != nil:
		return err
	}
	if *showVersion {
		fmt.Println(version.String())
		return nil
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	cfg, err := loader.Load()
	if err != nil {
		return fmt.Errorf("invalid exporter configuration: %w", err)
	}
	attribution := cfg.Attribution
	extractor, err := submitterextractor.New(submitterextractor.Config{
		Expression:              attribution.SubmittedByExpression,
		CostLimit:               attribution.Limits.CostLimit,
		MaxExpressionBytes:      attribution.Limits.MaxExpressionBytes,
		MaxIdentityBytes:        attribution.Limits.MaxIdentityBytes,
		InterruptCheckFrequency: attribution.Limits.InterruptCheckFrequency,
	})
	if err != nil {
		return fmt.Errorf("invalid submitter configuration: %w", err)
	}

	// Falls back to the in-cluster service account when no kubeconfig is found.
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return fmt.Errorf("kubernetes client configuration: %w", err)
	}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}
	options := cfg.KartaDefinitionSource.Options()
	inventory, err := kartadefinitioninventory.New(client, options)
	if err != nil {
		return fmt.Errorf("invalid Karta definition sources: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	inventoryDone := make(chan error, 1)
	go func() { inventoryDone <- inventory.Run(ctx) }()
	if err := inventory.WaitLoaded(ctx); err != nil && ctx.Err() == nil {
		stop()
		return fmt.Errorf("initial Karta definition load: %w", errors.Join(err, <-inventoryDone))
	}
	if ctx.Err() == nil {
		slog.Info("Karta metrics exporter scaffold initialized; metrics collection is not implemented",
			"version", version.String(), "submitterExtraction", submitterextractor.IsEnabled(extractor),
			"catalogDefinitions", options.Catalog.Enabled, "clusterDefinitions", options.Cluster.Enabled)
	}
	select {
	case <-ctx.Done():
		stop() // A second signal now ends the process.
		err = <-inventoryDone
	case err = <-inventoryDone:
		// Run returns only after ctx ends, so an early return is a failure.
		if err == nil && ctx.Err() == nil {
			err = errors.New("stopped before shutdown")
		}
	}
	if err != nil {
		return fmt.Errorf("karta definition inventory: %w", err)
	}
	slog.Info("Karta metrics exporter stopped")
	return nil
}
