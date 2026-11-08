package machine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	ptypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// PriceRegion is the region of the AWS Price List Query API that the adapter calls,
// at https://api.pricing.us-east-1.amazonaws.com. The API runs in a few regions only,
// and each of them returns the prices of every region.
const PriceRegion = "us-east-1"

// PriceAPI is the part of the AWS Price List Query API client that this package
// calls. *pricing.Client implements it; tests use a fake.
type PriceAPI interface {
	GetProducts(ctx context.Context, in *pricing.GetProductsInput, optFns ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

// errNoPriceClient is the error of Prices for a Client without a price client.
var errNoPriceClient = errors.New("report this defect in the EC2 machine adapter: its client has no price client")

// priceFilters are the filters of the GetProducts calls of Prices, after regionCode:
// on-demand Linux instances, virtual or bare metal, on shared hardware with no
// software, and not the price lines of a capacity reservation. AWS files a bare metal
// type, such as m7i.metal-24xl, under the product family "Compute Instance (bare
// metal)".
var priceFilters = []ptypes.Filter{
	{Type: ptypes.FilterTypeAnyOf, Field: aws.String("productFamily"), Value: aws.String("Compute Instance,Compute Instance (bare metal)")},
	termMatch("operatingSystem", "Linux"),
	termMatch("tenancy", "Shared"),
	termMatch("preInstalledSw", "NA"),
	termMatch("capacitystatus", "Used"),
}

// Prices returns the on-demand price in USD per hour of each instance type of the
// client's region that the AWS Price List Query API has, by type name, with paginated
// GetProducts calls of 100 products each for the service AmazonEC2 and the filters of
// [priceFilters]. A product of several hourly prices counts with the lowest, and a
// type of several products with its lowest price. A price that is not a finite number
// above 0 is left out. Errors from the call are *Error with the operation
// GetProducts.
func (c *Client) Prices(ctx context.Context) (map[string]float64, error) {
	if c.prices == nil {
		return nil, errNoPriceClient
	}
	in := &pricing.GetProductsInput{
		ServiceCode:   aws.String("AmazonEC2"),
		FormatVersion: aws.String("aws_v1"),
		MaxResults:    aws.Int32(100),
		Filters:       append([]ptypes.Filter{termMatch("regionCode", c.region)}, priceFilters...),
	}
	out := map[string]float64{}
	p := pricing.NewGetProductsPaginator(c.prices, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, wrap("GetProducts", err)
		}
		for _, doc := range page.PriceList {
			name, price, ok := productPrice(doc)
			if !ok {
				continue
			}
			if old, seen := out[name]; !seen || price < old {
				out[name] = price
			}
		}
	}
	return out, nil
}

// termMatch is a GetProducts filter of the type TERM_MATCH.
func termMatch(field, value string) ptypes.Filter {
	return ptypes.Filter{Type: ptypes.FilterTypeTermMatch, Field: aws.String(field), Value: aws.String(value)}
}

// product is the part of one price list document that productPrice reads.
type product struct {
	Product struct {
		Attributes struct {
			InstanceType string `json:"instanceType"`
		} `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				Unit         string            `json:"unit"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// productPrice reads one price list document of GetProducts: its instance type and
// its lowest on-demand price in USD per hour that is a
// finite number above 0. ok is false for a document that
// is not JSON, that has no instance type, or that has no such price.
func productPrice(doc string) (name string, price float64, ok bool) {
	var p product
	if json.Unmarshal([]byte(doc), &p) != nil || p.Product.Attributes.InstanceType == "" {
		return "", 0, false
	}
	for _, term := range p.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if dim.Unit != "Hrs" {
				continue
			}
			usd, err := strconv.ParseFloat(dim.PricePerUnit["USD"], 64)
			if err != nil || !(usd > 0) || math.IsInf(usd, 0) {
				continue
			}
			if !ok || usd < price {
				price, ok = usd, true
			}
		}
	}
	return p.Product.Attributes.InstanceType, price, ok
}
