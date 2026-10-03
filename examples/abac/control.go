package main

import (
	"context"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/service/s3control"
	smithyendpoints "github.com/aws/smithy-go/endpoints"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// AWS S3 Control normally prefixes the endpoint with the account ID. The
// gateway serves S3 Control at the same host as S3, including loopback IPs.
func gatewayControlEndpoint(o *s3control.Options) {
	o.EndpointResolverV2 = controlEndpoint{url: *o.BaseEndpoint}
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("GatewayControlEndpoint", func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
			return next.HandleInitialize(smithyhttp.DisableEndpointHostPrefix(ctx, true), in)
		}), middleware.Before)
	})
}

// Preserve the SDK's S3 signing properties, including path escaping, while
// removing the account prefix added by its endpoint rules.
type controlEndpoint struct{ url string }

func (e controlEndpoint) ResolveEndpoint(ctx context.Context, params s3control.EndpointParameters) (smithyendpoints.Endpoint, error) {
	endpoint, err := s3control.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, params)
	if err != nil {
		return smithyendpoints.Endpoint{}, err
	}
	u, err := url.Parse(e.url)
	if err != nil {
		return smithyendpoints.Endpoint{}, err
	}
	endpoint.URI.Host = u.Host
	return endpoint, nil
}
