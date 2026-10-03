// Exchange a JWT through STS and demonstrate bucket-tag authorization.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/examples/internal/example"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
	controltypes "github.com/aws/aws-sdk-go-v2/service/s3control/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

func main() {
	o := example.Flags()
	file := flag.String("token-file", "", "JWT file (default: fetch from the local demo issuer)")
	role := flag.String("role", "arn:aws:iam::000000000000:role/storage", "role configured in the gateway identity policy")
	example.Main(o, func(ctx context.Context) error { return run(ctx, *o, *file, *role) })
}

func run(ctx context.Context, o example.Options, tokenFile, role string) error {
	admin, err := o.Client()
	if err != nil {
		return err
	}
	var raw []byte
	if tokenFile != "" {
		raw, err = os.ReadFile(tokenFile)
	} else {
		req, e := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:9003/token", nil)
		if e != nil {
			return e
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			return fmt.Errorf("start the demo issuer first: %w", e)
		}
		raw, err = io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("issuer returned HTTP %d", res.StatusCode)
		}
	}
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(raw))
	identity := sts.New(sts.Options{Region: o.Region, BaseEndpoint: &o.Endpoint, Credentials: aws.AnonymousCredentials{}})
	session, err := identity.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{RoleArn: &role, RoleSessionName: aws.String("abac-example"), WebIdentityToken: &token, DurationSeconds: aws.Int32(900)})
	if err != nil {
		return err
	}
	cred := session.Credentials
	if cred == nil {
		return fmt.Errorf("STS returned no credentials")
	}
	user := s3.New(s3.Options{Region: o.Region, BaseEndpoint: &o.Endpoint, UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider(aws.ToString(cred.AccessKeyId), aws.ToString(cred.SecretAccessKey), aws.ToString(cred.SessionToken))})
	fmt.Println("Exchanged JWT for temporary S3 credentials (including the session token)")
	control := s3control.New(s3control.Options{Region: o.Region, BaseEndpoint: &o.Endpoint, Credentials: admin.Options().Credentials}, gatewayControlEndpoint)
	buckets := make(map[string]string)
	for _, team := range []string{"analytics", "finance"} {
		b, cleanup, err := example.NewBucket(ctx, admin, "example-abac-"+team)
		if err != nil {
			return err
		}
		defer cleanup()
		buckets[team] = b
		_, err = admin.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: &b, Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("team"), Value: &team}}}})
		if err != nil {
			return err
		}
		_, err = admin.PutBucketAbac(ctx, &s3.PutBucketAbacInput{Bucket: &b, AbacStatus: &types.AbacStatus{Status: types.BucketAbacStatusEnabled}})
		if err != nil {
			return err
		}
		_, err = admin.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: aws.String("report.json"), Body: strings.NewReader(`{"report":"example"}`), Tagging: aws.String("format=json")})
		if err != nil {
			return err
		}
	}
	analytics, finance := buckets["analytics"], buckets["finance"]
	read := func(bucket string) error {
		res, err := user.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: aws.String("report.json")})
		if err != nil {
			return err
		}
		defer res.Body.Close()
		_, err = io.Copy(io.Discard, res.Body)
		return err
	}
	if err = read(analytics); err != nil {
		return err
	}
	fmt.Println("Allowed: analytics JWT reads the team=analytics bucket")
	if err = denied(read(finance)); err != nil {
		return err
	}
	fmt.Println("Denied: the same JWT cannot read the team=finance bucket")
	// Object tags are application metadata; this policy consults bucket tags.
	_, err = user.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: &analytics, Key: aws.String("report.json"), Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("team"), Value: aws.String("finance")}}}})
	if err != nil {
		return err
	}
	tags, err := user.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &analytics, Key: aws.String("report.json")})
	if err != nil {
		return err
	}
	if len(tags.TagSet) != 1 || aws.ToString(tags.TagSet[0].Value) != "finance" {
		return fmt.Errorf("object tags differ")
	}
	if err = read(analytics); err != nil {
		return err
	}
	fmt.Println("Object team=finance tag round-trips; access still follows the bucket's team=analytics tag")
	_, err = user.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: &finance, Key: aws.String("report.json"), Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("team"), Value: aws.String("analytics")}}}})
	if err = denied(err); err != nil {
		return err
	}
	// Even a role that can write data cannot administer bucket access tags.
	_, err = user.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: &analytics, Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("team"), Value: aws.String("finance")}}}})
	if err = denied(err); err != nil {
		return err
	}
	signed, err := s3.NewPresignClient(user).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &analytics, Key: aws.String("report.json")}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		return err
	}
	// Once ABAC is enabled, administrators change bucket tags with S3 Control.
	arn := "arn:aws:s3:::" + analytics
	_, err = control.TagResource(ctx, &s3control.TagResourceInput{AccountId: aws.String("000000000000"), ResourceArn: &arn, Tags: []controltypes.Tag{{Key: aws.String("team"), Value: aws.String("finance")}}})
	if err != nil {
		return err
	}
	if err = denied(read(analytics)); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, signed.Method, signed.URL, nil)
	if err != nil {
		return err
	}
	req.Header = signed.SignedHeader.Clone()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		return fmt.Errorf("expected revoked presigned URL to return 403, got %d", res.StatusCode)
	}
	fmt.Println("Retagged with S3 Control: existing credentials and presigned URL now receive AccessDenied")
	fmt.Println("Cleaning up both demo buckets")
	return nil
}

func denied(err error) error {
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "AccessDenied" {
		return nil
	}
	return fmt.Errorf("expected AccessDenied, got %v", err)
}
