package gateway_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/adrianliechti/s3-gateway/internal/identity"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
	controltypes "github.com/aws/aws-sdk-go-v2/service/s3control/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/golang-jwt/jwt/v5"
)

const readerRole = "arn:aws:iam::000000000000:role/reader"

func TestSDKCompatibilityWebIdentityABAC(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	defer issuer.Close()
	cfg := identity.Config{Issuer: issuer.URL, JWKSURL: issuer.URL, Audience: "gateway", RolesClaim: "realm_access.roles", AllowInsecureHTTP: true, PrincipalTags: map[string]string{"team": "team"}, Roles: map[string]identity.Role{readerRole: {ClaimValue: "reader", Policy: identity.Policy{Version: "2012-10-17", Statement: []identity.Statement{
		{Effect: "Allow", Action: identity.Strings{"s3:ListAllMyBuckets"}, Resource: identity.Strings{"*"}},
		{Effect: "Allow", Action: identity.Strings{"s3:ListBucket", "s3:GetObject*", "s3:PutObject", "s3:DeleteObject*", "s3:ListMultipartUploadParts", "s3:AbortMultipartUpload"}, Resource: identity.Strings{"arn:aws:s3:::*"}, Condition: map[string]map[string]identity.Strings{"StringEquals": {"aws:ResourceTag/team": identity.Strings{"${aws:PrincipalTag/team}"}}}},
		{Effect: "Deny", Action: identity.Strings{"*"}, Resource: identity.Strings{"arn:aws:s3:::*/private/*"}},
	}}}}}
	g, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret, Identity: &cfg})
	must(t, err)
	tr := &transport{handler: g}
	admin := client(tr)
	stsClient := sts.New(sts.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://gateway.test"), HTTPClient: &http.Client{Transport: tr}, RetryMaxAttempts: 1, Credentials: aws.AnonymousCredentials{}})
	sign := func(claims jwt.MapClaims) string {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test"
		raw, err := token.SignedString(key)
		must(t, err)
		return raw
	}
	claims := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": issuer.URL, "aud": "gateway", "sub": "subject-123", "exp": time.Now().Add(time.Hour).Unix(), "team": "blue", "realm_access": map[string]any{"roles": []string{"reader"}}}
	}
	exchange := func(raw, role string) (aws.Credentials, error) {
		out, err := stsClient.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{RoleArn: &role, RoleSessionName: aws.String("sdk-session"), WebIdentityToken: &raw, DurationSeconds: aws.Int32(900)})
		if err != nil {
			return aws.Credentials{}, err
		}
		if out.Credentials == nil || out.AssumedRoleUser == nil || aws.ToString(out.SubjectFromWebIdentityToken) != "subject-123" {
			t.Fatal("incomplete STS response")
		}
		return aws.Credentials{AccessKeyID: aws.ToString(out.Credentials.AccessKeyId), SecretAccessKey: aws.ToString(out.Credentials.SecretAccessKey), SessionToken: aws.ToString(out.Credentials.SessionToken)}, nil
	}
	for _, change := range []func(jwt.MapClaims){func(c jwt.MapClaims) { c["iss"] = "https://untrusted.example" }, func(c jwt.MapClaims) { c["aud"] = "wrong" }, func(c jwt.MapClaims) { delete(c, "exp") }, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() }} {
		c := claims()
		change(c)
		_, err := exchange(sign(c), readerRole)
		code(t, err, "InvalidIdentityToken")
	}
	c := claims()
	c["realm_access"] = map[string]any{"roles": []string{"other"}}
	_, err = exchange(sign(c), readerRole)
	code(t, err, "AccessDenied")
	_, err = exchange(sign(claims()), "arn:aws:iam::000000000000:role/admin")
	code(t, err, "AccessDenied")
	credentials, err := exchange(sign(claims()), readerRole)
	must(t, err)
	makeClient := func(creds aws.Credentials) *s3.Client {
		return s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://gateway.test"), UsePathStyle: true, HTTPClient: &http.Client{Transport: tr}, RetryMaxAttempts: 1, Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return creds, nil })})
	}
	cUser := makeClient(credentials)
	_, err = admin.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: &f.bucket, Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("team"), Value: aws.String("blue")}}}})
	must(t, err)
	_, err = admin.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: aws.String("allowed"), Body: strings.NewReader("data")})
	must(t, err)
	_, err = cUser.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: aws.String("allowed")})
	code(t, err, "AccessDenied") // ABAC opt-in.
	_, err = admin.PutBucketAbac(ctx, &s3.PutBucketAbacInput{Bucket: &f.bucket, AbacStatus: &types.AbacStatus{Status: "Enabled"}})
	must(t, err)
	status, err := admin.GetBucketAbac(ctx, &s3.GetBucketAbacInput{Bucket: &f.bucket})
	must(t, err)
	if status.AbacStatus.Status != "Enabled" {
		t.Fatal("ABAC status not persisted")
	}
	_, err = admin.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: &f.bucket})
	code(t, err, "InvalidRequest")
	_, err = cUser.PutBucketAbac(ctx, &s3.PutBucketAbacInput{Bucket: &f.bucket, AbacStatus: &types.AbacStatus{Status: "Disabled"}})
	code(t, err, "AccessDenied")
	read := func(c *s3.Client, k string) {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &k})
		must(t, err)
		_, err = io.Copy(io.Discard, out.Body)
		out.Body.Close()
		must(t, err)
	}
	read(cUser, "allowed")
	control := s3control.New(s3control.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://gateway.test"), HTTPClient: &http.Client{Transport: tr}, RetryMaxAttempts: 1, Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, nil
	})})
	arn := "arn:aws:s3:::" + f.bucket
	_, err = control.TagResource(ctx, &s3control.TagResourceInput{AccountId: aws.String("000000000000"), ResourceArn: &arn, Tags: []controltypes.Tag{{Key: aws.String("sdk"), Value: aws.String("works")}}})
	must(t, err)
	tags, err := control.ListTagsForResource(ctx, &s3control.ListTagsForResourceInput{AccountId: aws.String("000000000000"), ResourceArn: &arn})
	must(t, err)
	if len(tags.Tags) != 2 {
		t.Fatal("S3 Control tags missing")
	}
	_, err = control.UntagResource(ctx, &s3control.UntagResourceInput{AccountId: aws.String("000000000000"), ResourceArn: &arn, TagKeys: []string{"sdk"}})
	must(t, err)
	_, err = cUser.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: aws.String("private/key")})
	code(t, err, "AccessDenied")
	// Sessions survive process restart with the same root secret and policy.
	g2, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret, Identity: &cfg})
	must(t, err)
	tr.handler = g2
	read(cUser, "allowed")
	for _, creds := range []aws.Credentials{{AccessKeyID: credentials.AccessKeyID, SecretAccessKey: credentials.SecretAccessKey}, {AccessKeyID: credentials.AccessKeyID, SecretAccessKey: credentials.SecretAccessKey, SessionToken: credentials.SessionToken + "a"}, {AccessKeyID: access, SecretAccessKey: secret, SessionToken: credentials.SessionToken}} {
		_, err = makeClient(creds).GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: aws.String("allowed")})
		code(t, err, "InvalidToken")
	}
	_, err = cUser.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &f.bucket, Key: aws.String("copy"), CopySource: aws.String(f.bucket + "/private/key")})
	code(t, err, "AccessDenied")
	_, err = cUser.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: aws.String("allowed"), Body: strings.NewReader("data"), Tagging: aws.String("team=red")})
	code(t, err, "AccessDenied")
	batch, err := cUser.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &f.bucket, Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("private/key")}, {Key: aws.String("missing")}}}})
	must(t, err)
	if len(batch.Errors) != 1 || aws.ToString(batch.Errors[0].Code) != "AccessDenied" || len(batch.Deleted) != 1 {
		t.Fatalf("incorrect batch authorization: %+v", batch)
	}
	init, err := cUser.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: aws.String("upload")})
	must(t, err)
	_, err = cUser.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &f.bucket, Key: aws.String("upload"), UploadId: init.UploadId, PartNumber: aws.Int32(1), CopySource: aws.String(f.bucket + "/private/key")})
	code(t, err, "AccessDenied")
	_, err = cUser.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &f.bucket, Key: aws.String("upload"), UploadId: init.UploadId})
	must(t, err)
	// Bucket tag changes immediately affect existing credentials and URLs.
	presigned, err := s3.NewPresignClient(cUser).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: aws.String("allowed")})
	must(t, err)
	request, err := http.NewRequest("GET", presigned.URL, nil)
	must(t, err)
	response, err := tr.RoundTrip(request)
	must(t, err)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("presign failed: %d", response.StatusCode)
	}
	must(t, f.be.(backend.Properties).SetBucketProperties(ctx, f.bucket, backend.BucketProperties{ABAC: "Enabled", Tags: []backend.Tag{{Key: "team", Value: "red"}}}))
	response, err = tr.RoundTrip(request)
	must(t, err)
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("presign bypassed tag revocation")
	}
	buckets, err := cUser.ListBuckets(ctx, &s3.ListBucketsInput{})
	must(t, err)
	for _, b := range buckets.Buckets {
		if aws.ToString(b.Name) == f.bucket {
			t.Fatal("unauthorized bucket disclosed")
		}
	}
	// Verify resource tagging uses the S3 Control shape with SigV4.
	raw := `<TagResourceRequest xmlns="http://awss3control.amazonaws.com/doc/2018-08-20/"><Tags><Tag><Key>team</Key><Value>blue</Value></Tag></Tags></TagResourceRequest>`
	request, err = http.NewRequest("POST", "http://gateway.test/v20180820/tags/arn:aws:s3:::"+f.bucket, strings.NewReader(raw))
	must(t, err)
	request.Header.Set("X-Amz-Account-Id", "000000000000")
	request.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	must(t, v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, request, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }))
	response, err = tr.RoundTrip(request)
	must(t, err)
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("resource tagging failed: %d", response.StatusCode)
	}
	read(cUser, "allowed")
}
