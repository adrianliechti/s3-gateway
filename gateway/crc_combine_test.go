package gateway

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestCombinedCRCMatchesWholeObject(t *testing.T) {
	for _, alg := range []string{"CRC32", "CRC32C", "CRC64NVME"} {
		t.Run(alg, func(t *testing.T) {
			whole := checksumHash(alg)
			var combined string
			for _, size := range []int{0, 1, 2, 31, 65537, 0, 8 << 20, 19} {
				data := bytes.Repeat([]byte{byte(size + 17)}, size)
				part := checksumHash(alg)
				part.Write(data)
				whole.Write(data)
				var err error
				combined, err = combineChecksum(alg, combined, base64.StdEncoding.EncodeToString(part.Sum(nil)), int64(size))
				if err != nil {
					t.Fatal(err)
				}
				if want := base64.StdEncoding.EncodeToString(whole.Sum(nil)); combined != want {
					t.Fatalf("size %d: got %s want %s", size, combined, want)
				}
			}
		})
	}
}
