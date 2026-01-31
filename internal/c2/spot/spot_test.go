package spot

import (
	"testing"
)

func TestParsePricingJSON(t *testing.T) {
	// Sample pricing JSON from AWS Pricing API (simplified but realistic structure)
	sampleJSON := `{
		"product": {
			"productFamily": "Compute Instance",
			"attributes": {
				"instanceType": "c6g.medium"
			}
		},
		"terms": {
			"OnDemand": {
				"ABCD1234.JRTCKXETXF": {
					"priceDimensions": {
						"ABCD1234.JRTCKXETXF.6YS6EN2CT7": {
							"unit": "Hrs",
							"pricePerUnit": {
								"USD": "0.0340000000"
							}
						}
					}
				}
			}
		}
	}`

	price, err := parsePricingJSON(sampleJSON)
	if err != nil {
		t.Fatalf("parsePricingJSON failed: %v", err)
	}

	expectedPrice := 0.034
	if price != expectedPrice {
		t.Errorf("expected price %.4f, got %.4f", expectedPrice, price)
	}
}

func TestParsePricingJSON_InvalidJSON(t *testing.T) {
	_, err := parsePricingJSON("not valid json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParsePricingJSON_MissingTerms(t *testing.T) {
	_, err := parsePricingJSON(`{"product": {}}`)
	if err == nil {
		t.Error("expected error for missing terms")
	}
}

func TestRegionLocations(t *testing.T) {
	// Verify common regions are mapped
	testCases := []struct {
		region   string
		expected string
	}{
		{"us-east-1", "US East (N. Virginia)"},
		{"us-west-2", "US West (Oregon)"},
		{"eu-west-1", "EU (Ireland)"},
		{"ap-northeast-1", "Asia Pacific (Tokyo)"},
	}

	for _, tc := range testCases {
		t.Run(tc.region, func(t *testing.T) {
			location, ok := regionLocations[tc.region]
			if !ok {
				t.Errorf("region %s not found in regionLocations", tc.region)
				return
			}
			if location != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, location)
			}
		})
	}
}

func TestSupportedRegionsHaveLocations(t *testing.T) {
	// Verify all supported regions have corresponding location mappings
	for _, region := range SupportedRegions {
		t.Run(region, func(t *testing.T) {
			if _, ok := regionLocations[region]; !ok {
				t.Errorf("supported region %s has no location mapping", region)
			}
		})
	}
}

func TestSupportedRegionsCount(t *testing.T) {
	// Ensure we have exactly 9 supported regions
	expected := 9
	if len(SupportedRegions) != expected {
		t.Errorf("expected %d supported regions, got %d", expected, len(SupportedRegions))
	}
}

func TestFallbackPrices(t *testing.T) {
	// Verify all instance types used in InstanceTypes have fallback prices
	allTypes := []map[string]map[string]InstancePair{InstanceTypes, InstanceTypesAlt1, InstanceTypesAlt2}

	for _, typeMap := range allTypes {
		for mode, archMap := range typeMap {
			for arch, pair := range archMap {
				t.Run(mode+"/"+arch, func(t *testing.T) {
					if _, ok := fallbackPrices[pair.Server]; !ok {
						t.Errorf("missing fallback price for server %s", pair.Server)
					}
					if _, ok := fallbackPrices[pair.Client]; !ok {
						t.Errorf("missing fallback price for client %s", pair.Client)
					}
				})
			}
		}
	}
}
