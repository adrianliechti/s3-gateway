package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/adrianliechti/s3-gateway/notification"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

type recordedSNS struct {
	mu       sync.Mutex
	messages []string
	fail     bool
}

func (p *recordedSNS) Publish(ctx context.Context, topic, message string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("SNS unavailable")
	}
	p.messages = append(p.messages, message)
	return nil
}
func (p *recordedSNS) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.messages) }

func TestSDKCompatibilitySNSNotifications(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	publisher := &recordedSNS{}
	g, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret, Notifications: publisher})
	must(t, err)
	tr := &transport{handler: g}
	c := client(tr)
	topic := "arn:aws:sns:us-east-1:000000000000:events"
	config := &types.NotificationConfiguration{TopicConfigurations: []types.TopicConfiguration{{Id: aws.String("events"), TopicArn: &topic, Events: []types.Event{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"}, Filter: &types.NotificationConfigurationFilter{Key: &types.S3KeyFilter{FilterRules: []types.FilterRule{{Name: types.FilterRuleNamePrefix, Value: aws.String("events/")}, {Name: types.FilterRuleNameSuffix, Value: aws.String(".json")}}}}}}}
	_, err = c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: &f.bucket, NotificationConfiguration: config})
	must(t, err)
	if publisher.count() != 1 {
		t.Fatal("destination test event not sent")
	}
	got, err := c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: &f.bucket})
	must(t, err)
	if len(got.TopicConfigurations) != 1 {
		t.Fatal("configuration not persisted")
	}
	publisher.fail = true
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: aws.String("events/a +雪.json"), Body: strings.NewReader("event")})
	must(t, err)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: aws.String("ignored"), Body: strings.NewReader("ignored")})
	must(t, err)
	if publisher.count() != 1 {
		t.Fatal("unexpected publish")
	}
	g, err = gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret, Notifications: publisher})
	must(t, err)
	tr.handler = g
	publisher.fail = false
	must(t, g.RunNotificationsOnce(ctx, time.Now()))
	must(t, g.RunNotificationsOnce(ctx, time.Now()))
	if publisher.count() != 2 {
		t.Fatal("restart did not retry exactly one pending message")
	}
	var message struct {
		Records []struct {
			EventName string `json:"eventName"`
			S3        struct {
				Object struct {
					Key, ETag string
					Size      int64
				} `json:"object"`
			} `json:"s3"`
		} `json:"Records"`
	}
	must(t, json.Unmarshal([]byte(publisher.messages[1]), &message))
	if len(message.Records) != 1 || message.Records[0].EventName != "ObjectCreated:Put" || message.Records[0].S3.Object.Key != "events%2Fa+%2B%E9%9B%AA.json" || message.Records[0].S3.Object.Size != 5 {
		t.Fatalf("incorrect event: %s", publisher.messages[1])
	}
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &f.bucket, Key: aws.String("events/copy.json"), CopySource: aws.String(f.bucket + "/ignored")})
	must(t, err)
	_, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &f.bucket, Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("events/copy.json")}, {Key: aws.String("ignored")}}}})
	must(t, err)
	if publisher.count() != 4 {
		t.Fatalf("copy/batch events missing: %d", publisher.count())
	}
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: aws.String("events/missing.json")})
	must(t, err)
	if publisher.count() != 4 {
		t.Fatal("missing object deletion emitted an event")
	}
	init, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: aws.String("events/multipart.json")})
	must(t, err)
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: &f.bucket, Key: aws.String("events/multipart.json"), UploadId: init.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("completed")})
	must(t, err)
	completion := &s3.CompleteMultipartUploadInput{Bucket: &f.bucket, Key: aws.String("events/multipart.json"), UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}}}
	_, err = c.CompleteMultipartUpload(ctx, completion)
	must(t, err)
	_, err = c.CompleteMultipartUpload(ctx, completion)
	must(t, err)
	if publisher.count() != 5 || !strings.Contains(publisher.messages[4], "ObjectCreated:CompleteMultipartUpload") {
		t.Fatal("multipart event or retry deduplication failed")
	}
	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &f.bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	must(t, err)
	deleted, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: aws.String("events/multipart.json")})
	must(t, err)
	if publisher.count() != 6 || !strings.Contains(publisher.messages[5], "ObjectRemoved:DeleteMarkerCreated") || !strings.Contains(publisher.messages[5], aws.ToString(deleted.VersionId)) {
		t.Fatal("delete marker event missing version")
	}
	// Atomic configuration replacement on failed destination validation.
	publisher.fail = true
	_, err = c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: &f.bucket, NotificationConfiguration: config})
	code(t, err, "InvalidArgument")
	got, err = c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: &f.bucket})
	must(t, err)
	if len(got.TopicConfigurations) != 1 {
		t.Fatal("failed validation replaced configuration")
	}
	publisher.fail = false
	_, err = c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: &f.bucket, NotificationConfiguration: &types.NotificationConfiguration{}})
	must(t, err)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: aws.String("events/disabled.json"), Body: strings.NewReader("none")})
	must(t, err)
	if publisher.count() != 6 {
		t.Fatal("disabled configuration emitted event")
	}
}

func TestSNSLive(t *testing.T) {
	endpoint := os.Getenv("GATEWAY_TEST_SNS_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GATEWAY_TEST_SNS_ENDPOINT and GATEWAY_SNS_ACCESS_KEY/SECRET_KEY")
	}
	ctx := t.Context()
	accessKey, secretKey := os.Getenv("GATEWAY_SNS_ACCESS_KEY"), os.Getenv("GATEWAY_SNS_SECRET_KEY")
	api := sns.New(sns.Options{Region: "us-east-1", BaseEndpoint: &endpoint, Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""), RetryMaxAttempts: 1})
	out, err := api.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String(fmt.Sprintf("s3-gateway-test-%d", time.Now().UnixNano()))})
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := api.DeleteTopic(ctx, &sns.DeleteTopicInput{TopicArn: out.TopicArn})
		if err != nil {
			t.Error(err)
		}
	})
	type envelope struct{ Type, Token, Message string }
	messages := make(chan envelope, 10)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e envelope
		if json.NewDecoder(r.Body).Decode(&e) != nil {
			w.WriteHeader(400)
			return
		}
		select {
		case messages <- e:
		default:
			w.WriteHeader(503)
		}
	}))
	defer webhook.Close()
	_, err = api.Subscribe(ctx, &sns.SubscribeInput{TopicArn: out.TopicArn, Protocol: aws.String("http"), Endpoint: &webhook.URL})
	must(t, err)
	wait := func() envelope {
		select {
		case e := <-messages:
			return e
		case <-time.After(20 * time.Second):
			t.Fatal("SNS delivery timed out")
			return envelope{}
		}
	}
	confirmation := wait()
	if confirmation.Type != "SubscriptionConfirmation" {
		t.Fatal("expected subscription confirmation")
	}
	_, err = api.ConfirmSubscription(ctx, &sns.ConfirmSubscriptionInput{TopicArn: out.TopicArn, Token: &confirmation.Token})
	must(t, err)
	publisher, err := notification.NewSNS(ctx, notification.Options{Endpoint: endpoint, Region: "us-east-1", AccessKey: accessKey, SecretKey: secretKey})
	must(t, err)
	f := versionSetup(t)
	g, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret, Notifications: publisher})
	must(t, err)
	c := client(&transport{handler: g})
	_, err = c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: &f.bucket, NotificationConfiguration: &types.NotificationConfiguration{TopicConfigurations: []types.TopicConfiguration{{TopicArn: out.TopicArn, Events: []types.Event{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"}}}}})
	must(t, err)
	testEvent := wait()
	if !strings.Contains(testEvent.Message, "s3:TestEvent") {
		t.Fatal("missing S3 destination test event")
	}
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: aws.String("native event.json"), Body: strings.NewReader("SNS end to end")})
	must(t, err)
	event := wait()
	if event.Type != "Notification" || !strings.Contains(event.Message, "ObjectCreated:Put") || !strings.Contains(event.Message, "native+event.json") {
		t.Fatal("missing object creation notification")
	}
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: aws.String("native event.json")})
	must(t, err)
	event = wait()
	if !strings.Contains(event.Message, "ObjectRemoved:Delete") {
		t.Fatal("missing object deletion notification")
	}
	// Disabling also keeps fixture cleanup from generating late webhook calls.
	_, err = c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: &f.bucket, NotificationConfiguration: &types.NotificationConfiguration{}})
	must(t, err)
	read, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &f.bucket})
	must(t, err)
	if len(read.Contents) != 0 {
		t.Fatal("notification helpers leaked through listing")
	}
	t.Log("S3 configuration → SNS Publish → confirmed HTTP subscriber verified")
}
