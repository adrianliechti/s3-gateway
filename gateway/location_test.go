package gateway

import (
	"net/url"
	"testing"
)

func TestCompletionLocationPreservesAuthorityAndKey(t *testing.T) {
	base, _ := url.Parse("https://bucket.gateway.test/upload?uploadId=internal&X-Amz-Signature=secret")
	for _, path := range []string{"/folder/a space+%雪", "//private-provider.invalid/key", "/a/../b", "/./a", "/..", "/a//b"} {
		t.Run(path, func(t *testing.T) {
			location, err := url.Parse(completionLocation(path))
			if err != nil {
				t.Fatal(err)
			}
			target := base.ResolveReference(location)
			if target.Host != base.Host || target.Scheme != base.Scheme || target.Path != path || target.RawQuery != "" || target.Fragment != "" {
				t.Fatalf("completion URL changed authority/key or retained credentials: %s", target)
			}
		})
	}
}
