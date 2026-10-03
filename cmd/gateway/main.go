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
	"github.com/adrianliechti/s3-gateway/backend/aws"
	"github.com/adrianliechti/s3-gateway/backend/azure"
	"github.com/adrianliechti/s3-gateway/backend/disk"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/adrianliechti/s3-gateway/internal/config"
	"github.com/adrianliechti/s3-gateway/internal/identity"
	"github.com/adrianliechti/s3-gateway/notification"
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
	identityConfig, err := identity.Load(c.IdentityConfig)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var notifications notification.Publisher
	if c.SNSEnabled || c.SNSEndpoint != "" {
		region := c.SNSRegion
		if region == "" {
			region = c.Region
		}
		notifications, err = notification.NewSNS(ctx, notification.Options{Endpoint: c.SNSEndpoint, Region: region, AccessKey: c.SNSAccessKey, SecretKey: c.SNSSecretKey, SessionToken: c.SNSSessionToken})
		if err != nil {
			return err
		}
	}
	var be backend.Backend
	switch c.Backend {
	case "disk":
		be, err = disk.New(disk.Options{Root: c.Root})
	case "azure":
		be, err = azure.New(azure.Options{Account: c.AzureAccount, AccountKey: c.AzureKey, ServiceURL: c.AzureURL, SASToken: c.AzureSAS})
	case "s3":
		be, err = aws.New(ctx, aws.Options{Endpoint: c.S3Endpoint, Region: c.S3Region, UsePathStyle: c.S3PathStyle})
	}
	if err != nil {
		return err
	}
	defer be.Close()
	if c.LifecycleTestDay > 0 {
		slog.Warn("accelerated lifecycle clock enabled for tests", "day", c.LifecycleTestDay)
	}
	slog.Info("starting S3 gateway", "backend", c.Backend, "listen", c.Listen, "region", c.Region)
	err = gateway.Run(ctx, be, gateway.Options{
		Notifications: notifications,
		Identity:      identityConfig,
		Listen:        c.Listen, Region: c.Region, Domain: c.Domain,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey,
		CertFile: c.CertFile, KeyFile: c.KeyFile, ReadOnly: c.ReadOnly,
		TempDir: c.TempDir, LifecycleInterval: c.LifecycleInterval, LifecycleTestDay: c.LifecycleTestDay,
	})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}
