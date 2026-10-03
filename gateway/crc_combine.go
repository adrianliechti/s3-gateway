package gateway

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

// CRC concatenation applies the zero-byte operator in GF(2). CRC32, CRC32C
// and CRC64/NVME all use reflected polynomials and all-ones initial/final XORs.
// Length is the number of bytes in the right-hand part, never its checksum size.
func combineCRC(left, right uint64, length int64, polynomial uint64, width int) uint64 {
	if length == 0 {
		return left
	}
	times := func(matrix [64]uint64, vector uint64) uint64 {
		var sum uint64
		for i := 0; vector != 0; i++ {
			if vector&1 != 0 {
				sum ^= matrix[i]
			}
			vector >>= 1
		}
		return sum
	}
	square := func(matrix [64]uint64) [64]uint64 {
		var out [64]uint64
		for i := 0; i < width; i++ {
			out[i] = times(matrix, matrix[i])
		}
		return out
	}
	var op [64]uint64
	op[0] = polynomial
	for i := 1; i < width; i++ {
		op[i] = uint64(1) << uint(i-1)
	}
	op = square(square(square(op))) // one zero byte
	for length > 0 {
		if length&1 != 0 {
			left = times(op, left)
		}
		length >>= 1
		if length > 0 {
			op = square(op)
		}
	}
	return left ^ right
}

func combineChecksum(algorithm, left, right string, length int64) (string, error) {
	var poly uint64
	width := 32
	switch algorithm {
	case "CRC32":
		poly = 0xedb88320
	case "CRC32C":
		poly = 0x82f63b78
	case "CRC64NVME":
		poly = 0x9a6c9329ac4bc9b5
		width = 64
	default:
		return "", fmt.Errorf("unsupported full-object checksum %s", algorithm)
	}
	decode := func(value string) (uint64, error) {
		if value == "" {
			return 0, apiError("InvalidPart", 400, "Part checksum missing")
		}
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(raw) != width/8 {
			return 0, apiError("InvalidPart", 400, "Invalid part checksum")
		}
		if width == 32 {
			return uint64(binary.BigEndian.Uint32(raw)), nil
		}
		return binary.BigEndian.Uint64(raw), nil
	}
	var a uint64
	var err error
	if left != "" {
		a, err = decode(left)
		if err != nil {
			return "", err
		}
	}
	b, err := decode(right)
	if err != nil {
		return "", err
	}
	value := combineCRC(a, b, length, poly, width)
	raw := make([]byte, width/8)
	if width == 32 {
		binary.BigEndian.PutUint32(raw, uint32(value))
	} else {
		binary.BigEndian.PutUint64(raw, value)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
