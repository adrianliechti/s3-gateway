// Package notification publishes S3 event notifications to SNS destinations.
package notification

import (
	"context"
	"fmt"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

type Publisher interface {
	Publish(context.Context, string, string) error
}
type Options struct{ Endpoint, Region, AccessKey, SecretKey, SessionToken string }
type SNS struct{ client *sns.Client }

func NewSNS(ctx context.Context, o Options) (*SNS, error) {
	if o.Endpoint != "" {
		u, err := url.Parse(o.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("invalid SNS endpoint")
		}
	}
	if (o.AccessKey == "") != (o.SecretKey == "") {
		return nil, fmt.Errorf("SNS access and secret keys must be configured together")
	}
	options := []func(*config.LoadOptions) error{}
	if o.Region != "" {
		options = append(options, config.WithRegion(o.Region))
	}
	if o.AccessKey != "" {
		options = append(options, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(o.AccessKey, o.SecretKey, o.SessionToken)))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	client := sns.NewFromConfig(cfg, func(c *sns.Options) {
		if o.Endpoint != "" {
			c.BaseEndpoint = aws.String(o.Endpoint)
		}
	})
	return &SNS{client: client}, nil
}
func (s *SNS) Publish(ctx context.Context, topic, message string) error {
	_, err := s.client.Publish(ctx, &sns.PublishInput{TopicArn: &topic, Message: &message})
	return err
}
