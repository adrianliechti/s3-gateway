package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/backend/azure"
	"github.com/adrianliechti/s3-gateway/backend/disk"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/adrianliechti/s3-gateway/internal/config"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	c, err := config.Parse(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		return err
	}
	var be backend.Backend
	switch c.Backend {
	case "disk":
		be, err = disk.New(disk.Options{Root: c.Root})
	case "azure":
		be, err = azure.New(azure.Options{Account: c.AzureAccount, AccountKey: c.AzureKey, ServiceURL: c.AzureURL, SASToken: c.AzureSAS})
	}
	if err != nil {
		return err
	}
	defer be.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("starting S3 gateway", "backend", c.Backend, "listen", c.Listen, "region", c.Region)
	err = gateway.Run(ctx, be, gateway.Options{
		Listen: c.Listen, Region: c.Region, Domain: c.Domain,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey,
		CertFile: c.CertFile, KeyFile: c.KeyFile, ReadOnly: c.ReadOnly,
		TempDir: c.TempDir,
	})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}
