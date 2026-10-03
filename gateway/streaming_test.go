package gateway

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// Published AWS vectors exercise the wire format independently of our signer.
// https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sigv4-streaming.html
// https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sigv4-streaming-trailers.html
func TestAWSStreamingVectors(t *testing.T) {
	for _, tc := range []struct {
		name, seed string
		chunks     [3]string
		trailer    string
	}{
		{"signed", "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9", [3]string{"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648", "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497", "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"}, ""},
		{"signed-trailer", "106e2a8a18243abcf37539882f36619c00e2dfc72633413f02d3b74544bfeb8e", [3]string{"b474d8862b1487a5145d686f57f013e54db672cee1c953b3010fb58501ef5aa2", "1c1344b170168f8e65b41376b44b20fe354e373826ccbbe2c1d40a8cae51e5c7", "2ca2aba2005185cf7159c6277faf83795951dd77a3a99e6e65d5c9f85863f992"}, "x-amz-checksum-crc32c:sOO8/Q==\r\nx-amz-trailer-signature:d81f82fc3505edab99d459891051a732e8730629a2e4a59689829ca17fe2e435\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := mac(mac(mac(mac([]byte("AWS4wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"), "20130524"), "us-east-1"), "s3"), "aws4_request")
			sig := signature{key: key, date: "20130524T000000Z", scope: "20130524/us-east-1/s3/aws4_request", previous: tc.seed, payload: "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"}
			if tc.trailer != "" {
				sig.payload += "-TRAILER"
			}
			wire := fmt.Sprintf("10000;chunk-signature=%s\r\n%s\r\n400;chunk-signature=%s\r\n%s\r\n0;chunk-signature=%s\r\n%s\r\n", tc.chunks[0], strings.Repeat("a", 65536), tc.chunks[1], strings.Repeat("a", 1024), tc.chunks[2], tc.trailer)
			if tc.trailer != "" {
				s := sig
				// Interoperability form used by clients that emit LF checksum
				// lines followed by a separate CRLF before the signature.
				variant := strings.Replace(wire, "\r\nx-amz-trailer-signature:", "\n\r\nx-amz-trailer-signature:", 1)
				var dst bytes.Buffer
				if _, _, err := decodeChunks(&dst, strings.NewReader(variant), &s, 1<<20); err != nil {
					t.Fatal(err)
				}
				s = sig
				variant = strings.Replace(variant, "x-amz-trailer-signature:d81", "x-amz-trailer-signature:000", 1)
				if _, _, err := decodeChunks(&dst, strings.NewReader(variant), &s, 1<<20); err == nil {
					t.Fatal("corrupt trailer accepted")
				}
			}
			for _, corrupt := range []bool{false, true} {
				var dst bytes.Buffer
				s := sig
				input := wire
				if corrupt {
					input = strings.Replace(input, "\r\naaaa", "\r\nbaaa", 1)
				}
				n, trailers, err := decodeChunks(&dst, strings.NewReader(input), &s, 1<<20)
				if corrupt {
					if err == nil {
						t.Fatal("corrupt chunk accepted")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if n != 66560 || dst.String() != strings.Repeat("a", 66560) {
					t.Fatal("decoded bytes differ")
				}
				if tc.trailer != "" && trailers.Get("x-amz-checksum-crc32c") != "sOO8/Q==" {
					t.Fatal("checksum trailer lost")
				}
			}
		})
	}
}
