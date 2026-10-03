package iceberg_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	sqlcat "github.com/apache/iceberg-go/catalog/sql"
	iceio "github.com/apache/iceberg-go/io"
	_ "github.com/apache/iceberg-go/io/gocloud"
	"github.com/apache/iceberg-go/table"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/uptrace/bun/driver/sqliteshim"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func setup(t *testing.T) (string, iceberg.Properties, *s3.Client) {
	t.Helper()
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("S3_ENDPOINT is required")
	}
	access, secret := os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY")
	if access == "" || secret == "" {
		t.Fatal("S3_ACCESS_KEY and S3_SECRET_KEY are required")
	}
	props := iceberg.Properties{iceio.S3Region: "us-east-1", iceio.S3AccessKeyID: access, iceio.S3SecretAccessKey: secret, iceio.S3EndpointURL: endpoint, iceio.S3ForceVirtualAddressing: "false"}
	client := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: &endpoint, UsePathStyle: true, Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, nil
	})})
	bucket := fmt.Sprintf("iceberg-%d", time.Now().UnixNano())
	var err error
	for i := 0; i < 60; i++ {
		_, err = client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &bucket})
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: &bucket})
		for pages.HasMorePages() {
			p, e := pages.NextPage(ctx)
			if e != nil {
				t.Error(e)
				return
			}
			var objects []types.ObjectIdentifier
			for _, v := range p.Contents {
				objects = append(objects, types.ObjectIdentifier{Key: v.Key})
			}
			if len(objects) > 0 {
				_, e = client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &types.Delete{Objects: objects}})
				if e != nil {
					t.Error(e)
				}
			}
		}
		_, e := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
		if e != nil {
			t.Error(e)
		}
	})
	return bucket, props, client
}

// Exercises the real Iceberg FileIO implementation, including seek/ReaderAt
// calls used for Parquet footers. The large stream exercises its upload path.
func TestIcebergS3FileIO(t *testing.T) {
	bucket, props, _ := setup(t)
	base := "s3://" + bucket + "/table"
	fs, err := iceio.LoadFS(t.Context(), props, base)
	must(t, err)
	writer, ok := fs.(iceio.WriteFileIO)
	if !ok {
		t.Fatal("S3 FileIO lacks writes")
	}
	name := base + "/data/part 1.parquet"
	data := bytes.Repeat([]byte("PAR1abcdefgh"), 1<<20)
	out, err := writer.Create(name)
	must(t, err)
	_, err = out.ReadFrom(bytes.NewReader(data))
	if err != nil {
		out.Close()
		t.Fatal(err)
	}
	must(t, out.Close())
	file, err := fs.Open(name)
	must(t, err)
	defer file.Close()
	info, err := file.Stat()
	must(t, err)
	if info.Size() != int64(len(data)) {
		t.Fatal("incorrect object size")
	}
	tail := make([]byte, 32)
	_, err = file.ReadAt(tail, int64(len(data)-32))
	must(t, err)
	if !bytes.Equal(tail, data[len(data)-32:]) {
		t.Fatal("ReaderAt returned incorrect footer bytes")
	}
	_, err = file.Seek(17, io.SeekStart)
	must(t, err)
	part := make([]byte, 64)
	_, err = io.ReadFull(file, part)
	must(t, err)
	if !bytes.Equal(part, data[17:81]) {
		t.Fatal("seek returned incorrect bytes")
	}
	must(t, fs.Remove(name))
	if _, err = fs.Open(name); err == nil {
		t.Fatal("deleted object remains readable")
	}
}

// Uses the SQL catalog pattern from Apache's S3 integration suite, targeting
// our endpoint instead of starting its internal MinIO recipe.
func TestIcebergCatalogMetadataRoundTrip(t *testing.T) {
	bucket, props, client := setup(t)
	props["uri"] = ":memory:"
	props[sqlcat.DriverKey] = sqliteshim.ShimName
	props[sqlcat.DialectKey] = string(sqlcat.SQLite)
	props["type"] = "sql"
	props["warehouse"] = "s3://" + bucket + "/warehouse"
	cat, err := catalog.Load(t.Context(), "gateway", props)
	must(t, err)
	namespace := catalog.ToIdentifier("analytics")
	must(t, cat.CreateNamespace(t.Context(), namespace, nil))
	ident := catalog.ToIdentifier("analytics", "events")
	tbl, err := cat.CreateTable(t.Context(), ident, iceberg.NewSchema(0, iceberg.NestedField{Name: "id", Type: iceberg.PrimitiveTypes.Int32, Required: true, ID: 1}))
	must(t, err)
	if tbl == nil {
		t.Fatal("table is nil")
	}
	loaded, err := cat.LoadTable(t.Context(), ident)
	must(t, err)
	if loaded == nil {
		t.Fatal("loaded table is nil")
	}
	objects, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: &bucket})
	must(t, err)
	if len(objects.Contents) == 0 {
		t.Fatal("Iceberg wrote no metadata to S3")
	}
}

// Write real Parquet data, commit two snapshots through the SQL catalog, then
// reload and scan both current and historical snapshots through S3 FileIO.
func TestIcebergParquetAppendAndScan(t *testing.T) {
	bucket, props, client := setup(t)
	// SQL catalog commits load metadata FileIO from table properties.
	// These credentials belong only to the disposable Docker fixture.
	s3Props := maps.Clone(props)
	props["uri"] = ":memory:"
	props[sqlcat.DriverKey] = sqliteshim.ShimName
	props[sqlcat.DialectKey] = string(sqlcat.SQLite)
	props["type"] = "sql"
	props["warehouse"] = "s3://" + bucket + "/warehouse"
	cat, err := catalog.Load(t.Context(), "gateway", props)
	must(t, err)
	must(t, cat.CreateNamespace(t.Context(), catalog.ToIdentifier("analytics"), nil))
	ident := catalog.ToIdentifier("analytics", "rows")
	tbl, err := cat.CreateTable(t.Context(), ident, iceberg.NewSchema(0, iceberg.NestedField{Name: "id", Type: iceberg.PrimitiveTypes.Int32, Required: true, ID: 1}), catalog.WithProperties(s3Props))
	must(t, err)
	schema, err := table.SchemaToArrowSchema(tbl.Schema(), nil, true, false)
	must(t, err)
	var firstSnapshot int64
	for batch := 0; batch < 2; batch++ {
		builder := array.NewInt32Builder(memory.DefaultAllocator)
		for i := 0; i < 1000; i++ {
			builder.Append(int32(batch*1000 + i))
		}
		values := builder.NewArray()
		builder.Release()
		rec := array.NewRecordBatch(schema, []arrow.Array{values}, 1000)
		values.Release()
		data := array.NewTableFromRecords(schema, []arrow.RecordBatch{rec})
		rec.Release()
		tbl, err = tbl.AppendTable(t.Context(), data, 256, nil)
		data.Release()
		must(t, err)
		if batch == 0 {
			firstSnapshot = tbl.CurrentSnapshot().SnapshotID
		}
	}
	loaded, err := cat.LoadTable(t.Context(), ident)
	must(t, err)
	if loaded.CurrentSnapshot().SnapshotID == firstSnapshot {
		t.Fatal("second append did not commit a new snapshot")
	}
	all, err := loaded.Scan().ToArrowTable(t.Context())
	must(t, err)
	defer all.Release()
	if all.NumRows() != 2000 {
		t.Fatalf("wrong row count: %d", all.NumRows())
	}
	var sum int64
	for _, chunk := range all.Column(0).Data().Chunks() {
		values := chunk.(*array.Int32)
		for i := 0; i < values.Len(); i++ {
			sum += int64(values.Value(i))
		}
	}
	if sum != 1999000 {
		t.Fatalf("Parquet values changed: sum=%d", sum)
	}
	historical, err := loaded.Scan(table.WithSnapshotID(firstSnapshot)).ToArrowTable(t.Context())
	must(t, err)
	defer historical.Release()
	if historical.NumRows() != 1000 {
		t.Fatal("historical snapshot changed after append")
	}
	filtered, err := loaded.Scan(table.WithRowFilter(iceberg.GreaterThanEqual(iceberg.Reference("id"), int32(1500)))).ToArrowTable(t.Context())
	must(t, err)
	defer filtered.Release()
	if filtered.NumRows() != 500 {
		t.Fatal("filtered Parquet scan returned incorrect rows")
	}
	objects, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: &bucket})
	must(t, err)
	parquet := 0
	for _, o := range objects.Contents {
		if strings.HasSuffix(aws.ToString(o.Key), ".parquet") {
			parquet++
		}
	}
	if parquet < 2 {
		t.Fatal("no real Parquet files written")
	}
}
