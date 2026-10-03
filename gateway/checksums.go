package gateway

// checksumValues is embedded in S3 XML responses. AWS and minio-go read these
// fields from the XML body of copy/completion/list responses, not HTTP headers.
type checksumValues struct {
	ChecksumCRC32     string `xml:",omitempty"`
	ChecksumCRC32C    string `xml:",omitempty"`
	ChecksumCRC64NVME string `xml:",omitempty"`
	ChecksumSHA1      string `xml:",omitempty"`
	ChecksumSHA256    string `xml:",omitempty"`
}

func xmlChecksums(m map[string]string) checksumValues {
	return checksumValues{m["CRC32"], m["CRC32C"], m["CRC64NVME"], m["SHA1"], m["SHA256"]}
}
