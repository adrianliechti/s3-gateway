package aws

import (
	"context"
	"fmt"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Request URL encoding on every provider listing, including maintenance scans.
// XML 1.0 cannot represent all valid S3 keys. Decode once before the paginator
// reuses key markers; continuation tokens and version IDs remain opaque.
type listingClient struct{ client *s3.Client }

func decodeListingFields(fields ...*string) error {
	for _, field := range fields {
		if field == nil {
			continue
		}
		decoded, err := url.QueryUnescape(*field)
		if err != nil {
			return fmt.Errorf("decode provider listing: %w", err)
		}
		*field = decoded
	}
	return nil
}

func (c listingClient) ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input, options ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	in := *input
	in.EncodingType = types.EncodingTypeUrl
	out, err := c.client.ListObjectsV2(ctx, &in, options...)
	if err != nil || out.EncodingType != types.EncodingTypeUrl {
		return out, err
	}
	fields := []*string{out.Prefix, out.Delimiter, out.StartAfter}
	for i := range out.Contents {
		fields = append(fields, out.Contents[i].Key)
	}
	for i := range out.CommonPrefixes {
		fields = append(fields, out.CommonPrefixes[i].Prefix)
	}
	return out, decodeListingFields(fields...)
}

func (c listingClient) ListObjectVersions(ctx context.Context, input *s3.ListObjectVersionsInput, options ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	in := *input
	in.EncodingType = types.EncodingTypeUrl
	out, err := c.client.ListObjectVersions(ctx, &in, options...)
	if err != nil || out.EncodingType != types.EncodingTypeUrl {
		return out, err
	}
	fields := []*string{out.Prefix, out.Delimiter, out.KeyMarker, out.NextKeyMarker}
	for i := range out.Versions {
		fields = append(fields, out.Versions[i].Key)
	}
	for i := range out.DeleteMarkers {
		fields = append(fields, out.DeleteMarkers[i].Key)
	}
	for i := range out.CommonPrefixes {
		fields = append(fields, out.CommonPrefixes[i].Prefix)
	}
	return out, decodeListingFields(fields...)
}
