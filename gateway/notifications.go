package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

const notificationPrefix = backend.InternalPrefix + "notifications/"

var notificationEvents = map[string]bool{"s3:ObjectCreated:*": true, "s3:ObjectCreated:Put": true, "s3:ObjectCreated:Copy": true, "s3:ObjectCreated:CompleteMultipartUpload": true, "s3:ObjectRemoved:*": true, "s3:ObjectRemoved:Delete": true, "s3:ObjectRemoved:DeleteMarkerCreated": true}

func notificationFilter(t backend.TopicConfiguration) (prefix, suffix string) {
	if t.Filter != nil && t.Filter.Key != nil {
		for _, rule := range t.Filter.Key.Rules {
			v, _ := url.QueryUnescape(rule.Value)
			if rule.Name == "prefix" {
				prefix = v
			} else {
				suffix = v
			}
		}
	}
	return
}
func notificationEventMatches(pattern, event string) bool {
	return pattern == event || strings.HasSuffix(pattern, "*") && strings.HasPrefix(event, strings.TrimSuffix(pattern, "*"))
}
func (g *Gateway) validateNotifications(doc *backend.NotificationConfiguration) error {
	if len(doc.Unknown) > 0 {
		return apiError("NotImplemented", 501, "Only SNS topic notifications are supported")
	}
	if len(doc.Topics) > 100 {
		return apiError("InvalidArgument", 400, "At most 100 notification rules are supported")
	}
	ids := map[string]bool{}
	for i := range doc.Topics {
		t := &doc.Topics[i]
		if len(t.Unknown) > 0 {
			return apiError("MalformedXML", 400, "Unknown notification field")
		}
		if t.ID == "" {
			t.ID = id()
		}
		if len(t.ID) > 255 || ids[t.ID] {
			return apiError("InvalidArgument", 400, "Notification IDs must be unique and at most 255 bytes")
		}
		ids[t.ID] = true
		parts := strings.Split(t.Topic, ":")
		if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "sns" || parts[3] != g.opts.Region || len(parts[4]) != 12 || strings.Trim(parts[4], "0123456789") != "" || parts[5] == "" || len(parts[5]) > 256 || strings.Trim(parts[5], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			return apiError("InvalidArgument", 400, "Expected a standard SNS topic ARN in the gateway region")
		}
		if len(t.Events) == 0 {
			return apiError("InvalidArgument", 400, "At least one event is required")
		}
		seen := map[string]bool{}
		for _, event := range t.Events {
			if !notificationEvents[event] {
				return apiError("NotImplemented", 501, "Unsupported notification event")
			}
			if seen[event] {
				return apiError("InvalidArgument", 400, "Duplicate notification event")
			}
			seen[event] = true
		}
		if t.Filter != nil {
			if len(t.Filter.Unknown) > 0 || t.Filter.Key == nil || len(t.Filter.Key.Unknown) > 0 {
				return apiError("MalformedXML", 400, "Invalid notification filter")
			}
			seen = map[string]bool{}
			for _, rule := range t.Filter.Key.Rules {
				_, err := url.QueryUnescape(rule.Value)
				if len(rule.Unknown) > 0 || rule.Name != "prefix" && rule.Name != "suffix" || seen[rule.Name] || len(rule.Value) > 1024 || err != nil || strings.Contains(rule.Value, "*") {
					return apiError("InvalidArgument", 400, "Invalid prefix or suffix filter")
				}
				seen[rule.Name] = true
			}
		}
	}
	for i, a := range doc.Topics {
		for _, b := range doc.Topics[i+1:] {
			ap, as := notificationFilter(a)
			bp, bs := notificationFilter(b)
			if !(strings.HasPrefix(ap, bp) || strings.HasPrefix(bp, ap)) || !(strings.HasSuffix(as, bs) || strings.HasSuffix(bs, as)) {
				continue
			}
			for _, ae := range a.Events {
				for _, be := range b.Events {
					if notificationEventMatches(ae, be) || notificationEventMatches(be, ae) {
						return apiError("InvalidArgument", 400, "Notification filters overlap for the same event")
					}
				}
			}
		}
	}
	return nil
}

func (g *Gateway) notificationConfiguration(w http.ResponseWriter, r *http.Request, b string, sig *signature) error {
	if err := g.be.HeadBucket(r.Context(), b); err != nil {
		return err
	}
	switch r.Method {
	case "GET":
		p, err := g.be.GetBucketProperties(r.Context(), b)
		if err != nil {
			return err
		}
		doc := p.Notifications
		if doc == nil {
			doc = &backend.NotificationConfiguration{}
		}
		doc.XMLNS = xmlns
		writeXML(w, 200, doc)
	case "PUT":
		var doc backend.NotificationConfiguration
		if err := g.configurationBody(r, sig, maxXMLSize, &doc); err != nil {
			return err
		}
		if err := g.validateNotifications(&doc); err != nil {
			return err
		}
		if len(doc.Topics) > 0 && g.opts.Notifications == nil {
			return apiError("InvalidRequest", 400, "SNS publishing is not configured")
		}
		skip := r.Header.Get("X-Amz-Skip-Destination-Validation")
		if skip != "" && skip != "true" && skip != "false" {
			return apiError("InvalidArgument", 400, "Invalid destination validation option")
		}
		if skip != "true" {
			raw, _ := json.Marshal(map[string]string{"Service": "Amazon S3", "Event": "s3:TestEvent", "Time": time.Now().UTC().Format(time.RFC3339Nano), "Bucket": b, "RequestId": w.Header().Get("x-amz-request-id"), "HostId": w.Header().Get("x-amz-id-2")})
			for _, t := range doc.Topics {
				ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
				err := g.opts.Notifications.Publish(ctx, t.Topic, string(raw))
				cancel()
				if err != nil {
					return apiError("InvalidArgument", 400, "Unable to validate SNS destination")
				}
			}
		}
		if err := g.be.UpdateBucketProperties(r.Context(), b, func(p *backend.BucketProperties) error { p.Notifications = &doc; return nil }); err != nil {
			return err
		}
		w.WriteHeader(200)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}

type notificationDelivery struct {
	Topic, Message string
	Delivered      bool
}
type notificationJob struct {
	Deliveries []notificationDelivery
	Created    time.Time
}

func (g *Gateway) checkPendingNotifications(ctx context.Context, b string) error {
	after := ""
	for {
		items, next, err := g.be.List(ctx, b, notificationPrefix, after, 1000)
		if err != nil {
			return err
		}
		for _, o := range items {
			_, body, err := g.be.Get(ctx, b, o.Key, backend.ReadOptions{Length: -1})
			if err != nil {
				return err
			}
			var job notificationJob
			err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&job)
			body.Close()
			if err != nil {
				return err
			}
			for _, d := range job.Deliveries {
				if !d.Delivered {
					return apiError("BucketNotEmpty", 409, "The bucket has pending SNS notifications")
				}
			}
		}
		if next == "" {
			return nil
		}
		after = next
	}
}

// Queue before acknowledging the HTTP operation. The outbox survives SNS
// failures and restarts. Object publication and outbox insertion are separate
// commits: a crash in that interval can lose an event (documented limitation).
func (g *Gateway) notifyObject(w http.ResponseWriter, r *http.Request, b, k, event, receipt string, o backend.Object) error {
	if strings.HasPrefix(event, "ObjectRemoved:") && o.Key == "" && !o.DeleteMarker {
		return nil
	}
	p, err := g.be.GetBucketProperties(r.Context(), b)
	if err != nil {
		return err
	}
	if p.Notifications == nil || len(p.Notifications.Topics) == 0 {
		return nil
	}
	g.notificationMu.Lock()
	defer g.notificationMu.Unlock()
	job := notificationJob{Created: time.Now().UTC()}
	principalID := g.owner().ID
	if who := principal(r); who != nil {
		principalID = who.Role + ":" + who.Subject
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	for _, topic := range p.Notifications.Topics {
		prefix, suffix := notificationFilter(topic)
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, suffix) {
			continue
		}
		match := false
		for _, pattern := range topic.Events {
			match = match || notificationEventMatches(pattern, "s3:"+event)
		}
		if !match {
			continue
		}
		object := map[string]any{"key": url.QueryEscape(k), "sequencer": fmt.Sprintf("%016X", job.Created.UnixNano())}
		if strings.HasPrefix(event, "ObjectCreated:") {
			object["size"] = o.Size
			object["eTag"] = o.ETag
		}
		if o.VersionID != "" {
			object["versionId"] = o.VersionID
		}
		raw, err := json.Marshal(map[string]any{"Records": []any{map[string]any{
			"eventVersion": "2.1", "eventSource": "aws:s3", "awsRegion": g.opts.Region, "eventTime": job.Created.Format(time.RFC3339Nano), "eventName": event,
			"userIdentity": map[string]string{"principalId": principalID}, "requestParameters": map[string]string{"sourceIPAddress": ip}, "responseElements": map[string]string{"x-amz-request-id": w.Header().Get("x-amz-request-id"), "x-amz-id-2": w.Header().Get("x-amz-id-2")},
			"s3": map[string]any{"s3SchemaVersion": "1.0", "configurationId": topic.ID, "bucket": map[string]any{"name": b, "arn": "arn:aws:s3:::" + b, "ownerIdentity": map[string]string{"principalId": g.owner().ID}}, "object": object},
		}}})
		if err != nil {
			return err
		}
		job.Deliveries = append(job.Deliveries, notificationDelivery{Topic: topic.Topic, Message: string(raw)})
	}
	if len(job.Deliveries) == 0 {
		return nil
	}
	if receipt == "" {
		receipt = id()
	}
	key := notificationPrefix + receipt
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = g.be.Put(r.Context(), b, key, bytes.NewReader(raw), int64(len(raw)), backend.PutOptions{Conditions: backend.Conditions{IfNoneMatch: "*"}})
	if errors.Is(err, backend.ErrPrecondition) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = g.deliverNotifications(r.Context(), b, key, job); err != nil {
		slog.Warn("SNS notification queued for retry", "bucket", b, "event", event)
	}
	return nil
}

func (g *Gateway) deliverNotifications(ctx context.Context, b, key string, job notificationJob) error {
	for i, d := range job.Deliveries {
		if d.Delivered {
			continue
		}
		if g.opts.Notifications == nil {
			return fmt.Errorf("SNS publisher not configured")
		}
		publishCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := g.opts.Notifications.Publish(publishCtx, d.Topic, d.Message)
		cancel()
		if err != nil {
			return err
		}
		job.Deliveries[i].Delivered = true
		raw, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if _, err = g.be.Put(ctx, b, key, bytes.NewReader(raw), int64(len(raw)), backend.PutOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// RunNotificationsOnce retries pending deliveries. Run schedules it alongside
// maintenance; embedders using New must schedule it explicitly.
func (g *Gateway) RunNotificationsOnce(ctx context.Context, now time.Time) error {
	if g.opts.ReadOnly {
		return nil
	}
	g.notificationMu.Lock()
	defer g.notificationMu.Unlock()
	buckets, err := g.be.ListBuckets(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, b := range buckets {
		after := ""
		for {
			items, next, err := g.be.List(ctx, b.Name, notificationPrefix, after, 1000)
			if err != nil {
				failures = append(failures, err)
				break
			}
			for _, o := range items {
				_, body, err := g.be.Get(ctx, b.Name, o.Key, backend.ReadOptions{Length: -1})
				if err != nil {
					failures = append(failures, err)
					continue
				}
				var job notificationJob
				err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&job)
				body.Close()
				if err != nil {
					failures = append(failures, err)
					continue
				}
				done := true
				for _, d := range job.Deliveries {
					done = done && d.Delivered
				}
				if done && job.Created.Before(now.Add(-24*time.Hour)) {
					err = g.be.Delete(ctx, b.Name, o.Key, backend.Conditions{})
				} else if !done {
					err = g.deliverNotifications(ctx, b.Name, o.Key, job)
				}
				if err != nil {
					failures = append(failures, err)
				}
			}
			if next == "" {
				break
			}
			after = next
		}
	}
	return errors.Join(failures...)
}
