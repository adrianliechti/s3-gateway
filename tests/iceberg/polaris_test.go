package iceberg_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	"github.com/apache/iceberg-go/table"
)

// iceberg-go v0.6.0 always requests vended credentials by default. Polaris's
// documented stsUnavailable mode requires omitting that optional request header.
// Limit the adapter to catalog requests; S3 traffic is untouched.
type noDelegation struct{ http.RoundTripper }

func (t noDelegation) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Del("X-Iceberg-Access-Delegation")
	return t.RoundTripper.RoundTrip(r)
}

func polarisRequest(t *testing.T, method, target, token string, payload any) []byte {
	t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		must(t, err)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	must(t, err)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		t.Fatalf("Polaris %s returned %d: %s", method, res.StatusCode, data)
	}
	return data
}

// Real Polaris REST catalog + its Java S3FileIO + iceberg-go's S3FileIO.
// Uses static fixture credentials and no STS, not remote signing or vending.
func TestPolarisCatalog(t *testing.T) {
	endpoint := os.Getenv("POLARIS_ENDPOINT")
	if endpoint == "" {
		t.Fatal("POLARIS_ENDPOINT is required")
	}
	bucket, props, client := setup(t)
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"PRINCIPAL_ROLE:ALL"}}
	req, err := http.NewRequestWithContext(t.Context(), "POST", endpoint+"/api/catalog/v1/oauth/tokens", bytes.NewBufferString(form.Encode()))
	must(t, err)
	req.SetBasicAuth("root", "test-secret")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	var auth struct {
		Token string `json:"access_token"`
	}
	err = json.NewDecoder(res.Body).Decode(&auth)
	res.Body.Close()
	must(t, err)
	if res.StatusCode != 200 || auth.Token == "" {
		t.Fatalf("Polaris token HTTP %d", res.StatusCode)
	}
	name := fmt.Sprintf("gateway-%d", time.Now().UnixNano())
	management := endpoint + "/api/management/v1/catalogs"
	polarisRequest(t, "POST", management, auth.Token, map[string]any{"catalog": map[string]any{
		"name": name, "type": "INTERNAL", "readOnly": false,
		"properties":        map[string]string{"default-base-location": "s3://" + bucket},
		"storageConfigInfo": map[string]any{"storageType": "S3", "allowedLocations": []string{"s3://" + bucket}, "endpoint": os.Getenv("GATEWAY_ENDPOINT"), "endpointInternal": os.Getenv("GATEWAY_ENDPOINT"), "region": "us-east-1", "pathStyleAccess": true, "stsUnavailable": true},
	}})
	// This grant follows Polaris's own no-STS S3 quickstart. Only disposable
	// Docker identities are used; production authorization is not under test.
	polarisRequest(t, "PUT", management+"/"+name+"/catalog-roles/catalog_admin/grants", auth.Token, map[string]string{"type": "catalog", "privilege": "CATALOG_MANAGE_CONTENT"})
	cat, err := rest.NewCatalog(t.Context(), name, endpoint+"/api/catalog",
		rest.WithOAuthToken(auth.Token), rest.WithWarehouseLocation(name),
		rest.WithAdditionalProps(props), rest.WithCustomTransport(noDelegation{http.DefaultTransport}))
	must(t, err)
	namespace := catalog.ToIdentifier("analytics")
	must(t, cat.CreateNamespace(t.Context(), namespace, nil))
	ident := catalog.ToIdentifier("analytics", "events")
	tbl, err := cat.CreateTable(t.Context(), ident, iceberg.NewSchema(0, iceberg.NestedField{Name: "id", Type: iceberg.PrimitiveTypes.Int32, Required: true, ID: 1}))
	must(t, err)
	checkAppendAndScan(t, cat, ident, tbl, client, bucket)

	// Independent clients start from the same snapshot and race commits.
	// Polaris must reject a stale snapshot; the caller reloads and retries.
	const writers = 2
	tables := make([]*table.Table, writers)
	for i := range tables {
		tables[i], err = cat.LoadTable(t.Context(), ident)
		must(t, err)
	}
	arrowSchema, err := table.SchemaToArrowSchema(tables[0].Schema(), nil, true, false)
	must(t, err)
	start := make(chan struct{})
	failures := make(chan error, writers)
	var wg sync.WaitGroup
	var conflicts atomic.Int32
	for i, current := range tables {
		wg.Add(1)
		go func() {
			defer wg.Done()
			builder := array.NewInt32Builder(memory.DefaultAllocator)
			for n := 0; n < 100; n++ {
				builder.Append(int32(2000 + i*100 + n))
			}
			values := builder.NewArray()
			builder.Release()
			rec := array.NewRecordBatch(arrowSchema, []arrow.Array{values}, 100)
			values.Release()
			data := array.NewTableFromRecords(arrowSchema, []arrow.RecordBatch{rec})
			rec.Release()
			defer data.Release()
			<-start
			_, err := current.AppendTable(t.Context(), data, 100, nil)
			if errors.Is(err, table.ErrCommitFailed) {
				conflicts.Add(1)
				latest, reloadErr := cat.LoadTable(t.Context(), ident)
				if reloadErr != nil {
					failures <- reloadErr
					return
				}
				_, err = latest.AppendTable(t.Context(), data, 100, nil)
			}
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		must(t, err)
	}
	if conflicts.Load() == 0 {
		t.Fatal("expected a stale snapshot commit conflict")
	}
	t.Logf("retried %d stale snapshot commit(s)", conflicts.Load())
	loaded, err := cat.LoadTable(t.Context(), ident)
	must(t, err)
	all, err := loaded.Scan().ToArrowTable(t.Context())
	must(t, err)
	defer all.Release()
	var sum int64
	for _, chunk := range all.Column(0).Data().Chunks() {
		v := chunk.(*array.Int32)
		for i := 0; i < v.Len(); i++ {
			sum += int64(v.Value(i))
		}
	}
	if all.NumRows() != 2200 || sum != 2418900 {
		t.Fatalf("concurrent appends lost/duplicated rows: count=%d sum=%d", all.NumRows(), sum)
	}
	must(t, cat.DropTable(t.Context(), ident))
	must(t, cat.DropNamespace(t.Context(), namespace))
	polarisRequest(t, "DELETE", management+"/"+name, auth.Token, nil)
}
