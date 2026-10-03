// Subscribe to local SNS, configure S3 events, then upload and delete an object.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/examples/internal/example"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

func main() {
	o := example.Flags()
	snsEndpoint := flag.String("sns-endpoint", example.Env("GATEWAY_SNS_ENDPOINT", "http://127.0.0.1:9001"), "local SNS gateway endpoint")
	listen := flag.String("listen", "127.0.0.1:9004", "HTTP subscriber listen address")
	callback := flag.String("callback-url", "http://127.0.0.1:9004/sns", "subscriber URL reachable by the SNS gateway")
	example.Main(o, func(ctx context.Context) error { return run(ctx, *o, *snsEndpoint, *listen, *callback) })
}

func run(ctx context.Context, o example.Options, snsEndpoint, listen, callback string) error {
	if err := example.Endpoint(snsEndpoint); err != nil {
		return err
	}
	u, err := url.Parse(callback)
	if err != nil || example.Endpoint(callback) != nil || u.Path != "/sns" {
		return fmt.Errorf("callback-url must be an HTTP(S) URL ending in /sns")
	}
	access, secret := os.Getenv("GATEWAY_SNS_ACCESS_KEY"), os.Getenv("GATEWAY_SNS_SECRET_KEY")
	if access == "" || secret == "" {
		return fmt.Errorf("set GATEWAY_SNS_ACCESS_KEY and GATEWAY_SNS_SECRET_KEY to the local SNS server's credentials")
	}
	c, err := o.Client()
	if err != nil {
		return err
	}
	notifications := sns.New(sns.Options{Region: o.Region, BaseEndpoint: &snsEndpoint, Credentials: credentials.NewStaticCredentialsProvider(access, secret, os.Getenv("GATEWAY_SNS_SESSION_TOKEN")), HTTPClient: &http.Client{Timeout: 15 * time.Second}})
	verify, err := LoadVerifier(ctx, snsEndpoint)
	if err != nil {
		return err
	}
	topic, err := notifications.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String(example.Name("example-s3-events"))})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := notifications.DeleteTopic(cleanup, &sns.DeleteTopicInput{TopicArn: topic.TopicArn}); err != nil {
			log.Printf("Delete demo topic: %v", err)
		}
	}()
	fmt.Printf("Created SNS topic %s\n", aws.ToString(topic.TopicArn))
	messages := make(chan Notification, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sns", func(w http.ResponseWriter, r *http.Request) {
		var n Notification
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if decoder.Decode(&n) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if n.TopicARN != aws.ToString(topic.TopicArn) || verify(n) != nil {
			http.Error(w, "invalid topic or signature", 403)
			return
		}
		select {
		case messages <- n:
			w.WriteHeader(204)
		default:
			http.Error(w, "subscriber busy", 503)
		}
	})
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}()
	_, err = notifications.Subscribe(ctx, &sns.SubscribeInput{TopicArn: topic.TopicArn, Protocol: &u.Scheme, Endpoint: &callback, ReturnSubscriptionArn: true})
	if err != nil {
		return err
	}
	wait := func() (Notification, error) {
		select {
		case n := <-messages:
			return n, nil
		case err := <-serverDone:
			return Notification{}, err
		case <-ctx.Done():
			return Notification{}, fmt.Errorf("waiting for SNS; check callback reachability and gateway SNS credentials: %w", ctx.Err())
		}
	}
	confirmation, err := wait()
	if err != nil {
		return err
	}
	if confirmation.Type != "SubscriptionConfirmation" {
		return fmt.Errorf("expected subscription confirmation")
	}
	// Never follow a SubscribeURL supplied in a webhook. Use the configured API.
	_, err = notifications.ConfirmSubscription(ctx, &sns.ConfirmSubscriptionInput{TopicArn: topic.TopicArn, Token: &confirmation.Token})
	if err != nil {
		return err
	}
	fmt.Println("Verified signature and confirmed HTTP subscription")

	b, cleanup, err := example.NewBucket(ctx, c, "example-sns")
	if err != nil {
		return err
	}
	defer cleanup()
	configuration := &types.NotificationConfiguration{
		TopicConfigurations: []types.TopicConfiguration{{
			Id:       aws.String("json-events"),
			TopicArn: topic.TopicArn,
			Events:   []types.Event{types.EventS3ObjectCreated, types.EventS3ObjectRemoved},
			Filter: &types.NotificationConfigurationFilter{
				Key: &types.S3KeyFilter{FilterRules: []types.FilterRule{
					{Name: types.FilterRuleNamePrefix, Value: aws.String("events/")},
					{Name: types.FilterRuleNameSuffix, Value: aws.String(".json")},
				}},
			},
		}},
	}
	_, err = c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
		Bucket: &b, NotificationConfiguration: configuration,
	})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := c.PutBucketNotificationConfiguration(cleanup, &s3.PutBucketNotificationConfigurationInput{Bucket: &b, NotificationConfiguration: &types.NotificationConfiguration{}})
		if err != nil {
			log.Printf("Disable demo notifications: %v", err)
		}
	}()
	key := "events/a space+plus.json"
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: &key, Body: strings.NewReader(`{"source":"s3-gateway"}`), ContentType: aws.String("application/json")})
	if err != nil {
		return err
	}
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: &key})
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for !seen["s3:TestEvent"] || !seen["ObjectCreated:Put"] || !seen["ObjectRemoved:Delete"] {
		n, err := wait()
		if err != nil {
			return err
		}
		if n.Type != "Notification" {
			continue
		} // tolerate a redelivered confirmation
		var event struct {
			Event, Bucket string
			Records       []struct {
				EventName string
				S3        struct {
					Bucket struct{ Name string }
					Object struct{ Key, VersionID, Sequencer string }
				}
			}
		}
		if err = json.Unmarshal([]byte(n.Message), &event); err != nil {
			return err
		}
		if event.Event == "s3:TestEvent" && event.Bucket == b {
			if !seen[event.Event] {
				fmt.Println("Received s3:TestEvent")
			}
			seen[event.Event] = true
		}
		for _, record := range event.Records {
			decoded, err := url.QueryUnescape(record.S3.Object.Key)
			if err != nil {
				return err
			}
			if record.S3.Bucket.Name != b || decoded != key {
				return fmt.Errorf("unexpected event object")
			}
			if !seen[record.EventName] {
				fmt.Printf("Received %s for %s (sequencer %s)\n", record.EventName, decoded, record.S3.Object.Sequencer)
			}
			seen[record.EventName] = true
		}
	}
	fmt.Println("SNS round trip complete; cleaning up bucket, subscription and topic")
	return nil
}
