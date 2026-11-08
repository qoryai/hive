package machine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	ptypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/qoryai/integrations/contracts"
)

// fakePrices records each GetProducts input and returns what get returns.
type fakePrices struct {
	mu  sync.Mutex
	ins []*pricing.GetProductsInput
	get func(*pricing.GetProductsInput) (*pricing.GetProductsOutput, error)
}

var _ PriceAPI = (*fakePrices)(nil)
var _ PriceAPI = (*pricing.Client)(nil)

func (f *fakePrices) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	f.mu.Lock()
	f.ins = append(f.ins, in)
	f.mu.Unlock()
	return f.get(in)
}

// priceDoc returns a price list document of GetProducts for the instance type name
// with one on-demand term whose price dimensions have these units and USD prices, of
// the product family "Compute Instance (bare metal)" for a name that contains metal,
// and "Compute Instance" for any other.
func priceDoc(name string, dims ...[2]string) string {
	family := "Compute Instance"
	if strings.Contains(name, "metal") {
		family = "Compute Instance (bare metal)"
	}
	d := map[string]any{}
	for i, u := range dims {
		d[string(rune('A'+i))] = map[string]any{"unit": u[0], "pricePerUnit": map[string]string{"USD": u[1]}}
	}
	b, err := json.Marshal(map[string]any{
		"product": map[string]any{"productFamily": family, "attributes": map[string]string{"instanceType": name}},
		"terms":   map[string]any{"OnDemand": map[string]any{"SKU.TERM": map[string]any{"priceDimensions": d}}},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestPrices pins the GetProducts input, the paging, the lowest hourly price of a
// type, the price of a bare metal type, and the documents that count with no price.
func TestPrices(t *testing.T) {
	pages := map[string]*pricing.GetProductsOutput{
		"": {
			PriceList: []string{
				priceDoc("t3.medium", [2]string{"Hrs", "0.0416000000"}),
				priceDoc("m7i.large", [2]string{"Hrs", "0.1008000000"}, [2]string{"Hrs", "0.0900000000"}),
			},
			NextToken: aws.String("page-2"),
		},
		"page-2": {PriceList: []string{
			priceDoc("m7i.large", [2]string{"Hrs", "0.0950000000"}),
			priceDoc("m7i.metal-24xl", [2]string{"Hrs", "4.8384000000"}),
			priceDoc("c7i.large", [2]string{"Quantity", "0.0100000000"}),
			priceDoc("m6i.large", [2]string{"Hrs", "0.0000000000"}),
			priceDoc("m5.large", [2]string{"Hrs", "NaN"}),
			priceDoc("", [2]string{"Hrs", "0.0100000000"}),
			"not json",
		}},
	}
	f := &fakePrices{get: func(in *pricing.GetProductsInput) (*pricing.GetProductsOutput, error) {
		return pages[aws.ToString(in.NextToken)], nil
	}}
	got, err := NewWithPrices(&fakeAPI{}, f, "eu-west-1").Prices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]float64{"t3.medium": 0.0416, "m7i.large": 0.09, "m7i.metal-24xl": 4.8384}; !reflect.DeepEqual(got, want) {
		t.Errorf("prices %v, want %v", got, want)
	}
	term := func(field, value string) ptypes.Filter {
		return ptypes.Filter{Type: ptypes.FilterTypeTermMatch, Field: aws.String(field), Value: aws.String(value)}
	}
	want := &pricing.GetProductsInput{
		ServiceCode:   aws.String("AmazonEC2"),
		FormatVersion: aws.String("aws_v1"),
		MaxResults:    aws.Int32(100),
		Filters: []ptypes.Filter{
			term("regionCode", "eu-west-1"),
			{Type: ptypes.FilterTypeAnyOf, Field: aws.String("productFamily"), Value: aws.String("Compute Instance,Compute Instance (bare metal)")},
			term("operatingSystem", "Linux"),
			term("tenancy", "Shared"),
			term("preInstalledSw", "NA"),
			term("capacitystatus", "Used"),
		},
	}
	if len(f.ins) != 2 || !reflect.DeepEqual(f.ins[0], want) {
		t.Errorf("%d calls, first input %+v", len(f.ins), f.ins[0])
	}

	f.get = func(*pricing.GetProductsInput) (*pricing.GetProductsOutput, error) {
		return nil, apiError("AccessDeniedException")
	}
	_, err = NewWithPrices(&fakeAPI{}, f, "eu-west-1").Prices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "GetProducts failed") || kindOf(err) != contracts.FailureOther {
		t.Errorf("denied: error %v, want a failed GetProducts", err)
	}

	if _, err := New(&fakeAPI{}).Prices(context.Background()); err == nil {
		t.Error("a client without a price client: no error")
	}
}

// TestWirePrices pins the GetProducts request on the wire: the JSON 1.1 protocol of
// the AWS Price List Query API, its target header, and the body with the filters.
func TestWirePrices(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var targets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		targets = append(targets, r.Header.Get("X-Amz-Target"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		doc, _ := json.Marshal(priceDoc("t3.medium", [2]string{"Hrs", "0.0416000000"}))
		io.WriteString(w, `{"FormatVersion": "aws_v1", "PriceList": [`+string(doc)+`]}`)
	}))
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:       "eu-west-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDTESTONLY", testSecret, ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
			})
		},
	}
	got, err := NewFromConfig(cfg).Prices(context.Background())
	if err != nil || !reflect.DeepEqual(got, map[string]float64{"t3.medium": 0.0416}) {
		t.Fatalf("prices %v, error %v", got, err)
	}
	if len(targets) != 1 || targets[0] != "AWSPriceListService.GetProducts" {
		t.Errorf("targets %v", targets)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &body); err != nil {
		t.Fatalf("body %q: %v", bodies[0], err)
	}
	if body["ServiceCode"] != "AmazonEC2" || body["FormatVersion"] != "aws_v1" || body["MaxResults"] != float64(100) {
		t.Errorf("body %s", bodies[0])
	}
	filters, _ := body["Filters"].([]any)
	first, _ := filters[0].(map[string]any)
	family, _ := filters[1].(map[string]any)
	if len(filters) != 6 || first["Type"] != "TERM_MATCH" || first["Field"] != "regionCode" || first["Value"] != "eu-west-1" ||
		family["Type"] != "ANY_OF" || family["Field"] != "productFamily" || family["Value"] != "Compute Instance,Compute Instance (bare metal)" {
		t.Errorf("filters %v", filters)
	}
}

// TestPriceEndpoint pins that the price client of NewFromConfig calls the standard
// endpoint in us-east-1, whatever the region of the configuration.
func TestPriceEndpoint(t *testing.T) {
	var host string
	cfg := aws.Config{
		Region:      "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDTESTONLY", testSecret, ""),
		HTTPClient: hostRecorder(func(h string) {
			host = h
		}),
		Retryer: func() aws.Retryer { return aws.NopRetryer{} },
	}
	_, _ = NewFromConfig(cfg).Prices(context.Background())
	if host != "api.pricing.us-east-1.amazonaws.com" {
		t.Errorf("host %q, want api.pricing.us-east-1.amazonaws.com", host)
	}
}

// hostRecorder is an HTTP client that records the host of each request and sends
// nothing.
type hostRecorder func(host string)

func (h hostRecorder) Do(r *http.Request) (*http.Response, error) {
	h(r.URL.Host)
	return nil, io.ErrUnexpectedEOF
}
