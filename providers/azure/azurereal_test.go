package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
)

// retailStub is a stub RoundTripper for the Retail Prices API that records the
// $filter and returns a fixed Linux+Windows spot meter set.
type retailStub struct{ lastFilter string }

func (s *retailStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.lastFilter = req.URL.Query().Get("$filter")
	body := `{"Items":[
		{"unitPrice":0.05,"meterName":"D4s_v5 Spot","productName":"Virtual Machines DSv5 Series","skuName":"D4s_v5 Spot"},
		{"unitPrice":0.04,"meterName":"D4s_v5 Spot","productName":"Virtual Machines DSv5 Series Windows","skuName":"D4s_v5 Spot"}
	]}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// SpotPriceUSD must filter on the FULL Standard_* armSkuName (regression: a
// stripped name matched zero meters and silently disabled live Spot pricing) and
// exclude the cheaper Windows meter, returning the Linux price.
func TestSpotPriceUSD_FullSkuNameAndExcludesWindows(t *testing.T) {
	stub := &retailStub{}
	a := &azureReal{
		cfg:      azureRealConfig{Location: "eastus"},
		priceAPI: "https://example.test/api",
		http:     &http.Client{Transport: stub},
	}
	got, err := a.SpotPriceUSD(context.Background(), "Standard_D4s_v5")
	if err != nil {
		t.Fatalf("SpotPriceUSD: %v", err)
	}
	if got != 0.05 {
		t.Errorf("price = %v, want 0.05 (Linux spot, not the 0.04 Windows meter)", got)
	}
	if !strings.Contains(stub.lastFilter, "armSkuName eq 'Standard_D4s_v5'") {
		t.Errorf("filter %q must use the full Standard_ sku name", stub.lastFilter)
	}
}

// onDemandStub returns a realistic mix of Consumption meters for one (region,
// SKU): Linux + Windows on-demand, Spot, and the retired Low Priority tier.
type onDemandStub struct{ lastFilter string }

func (s *onDemandStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.lastFilter = req.URL.Query().Get("$filter")
	body := `{"Items":[
		{"unitPrice":0.192,"meterName":"D4s_v5","productName":"Virtual Machines DSv5 Series","skuName":"D4s_v5"},
		{"unitPrice":0.350,"meterName":"D4s_v5","productName":"Virtual Machines DSv5 Series Windows","skuName":"D4s_v5"},
		{"unitPrice":0.040,"meterName":"D4s_v5 Spot","productName":"Virtual Machines DSv5 Series","skuName":"D4s_v5 Spot"},
		{"unitPrice":0.030,"meterName":"D4s_v5 Low Priority","productName":"Virtual Machines DSv5 Series","skuName":"D4s_v5 Low Priority"}
	]}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// OnDemandPriceUSD must return the Linux pay-as-you-go meter — never the cheaper
// Spot / Low Priority meters (which would understate on-demand cost) nor the
// Windows licence-surcharged meter — and filter on the full Standard_ armSkuName.
func TestOnDemandPriceUSD_SelectsLinuxPayAsYouGo(t *testing.T) {
	stub := &onDemandStub{}
	a := &azureReal{
		cfg:      azureRealConfig{Location: "eastus"},
		priceAPI: "https://example.test/api",
		http:     &http.Client{Transport: stub},
	}
	got, err := a.OnDemandPriceUSD(context.Background(), "Standard_D4s_v5")
	if err != nil {
		t.Fatalf("OnDemandPriceUSD: %v", err)
	}
	if got != 0.192 {
		t.Errorf("price = %v, want 0.192 (Linux on-demand, not Spot/Low Priority/Windows)", got)
	}
	if !strings.Contains(stub.lastFilter, "armSkuName eq 'Standard_D4s_v5'") {
		t.Errorf("filter %q must use the full Standard_ sku name", stub.lastFilter)
	}
}

// toVMInstance must derive the real power state from the expanded instance view:
// a deallocated VM is not Running even though provisioning Succeeded; a running
// VM is; and with no instance view it falls back to the Deleting heuristic.
func TestToVMInstance_PowerState(t *testing.T) {
	a := &azureReal{cfg: azureRealConfig{Location: "eastus"}}

	mk := func(props *armcompute.VirtualMachineProperties) armcompute.VirtualMachine {
		return armcompute.VirtualMachine{Location: to.Ptr("eastus"), Properties: props}
	}
	iv := func(power string) *armcompute.VirtualMachineProperties {
		return &armcompute.VirtualMachineProperties{
			InstanceView: &armcompute.VirtualMachineInstanceView{
				Statuses: []*armcompute.InstanceViewStatus{
					{Code: to.Ptr("ProvisioningState/succeeded")},
					{Code: to.Ptr(power)},
				},
			},
		}
	}

	if got := a.toVMInstance(mk(iv("PowerState/deallocated"))); got.Running {
		t.Error("deallocated VM reported Running")
	}
	if got := a.toVMInstance(mk(iv("PowerState/running"))); !got.Running {
		t.Error("running VM reported not Running")
	}
	// No instance view, ProvisioningState=Deleting -> fallback marks not running.
	deleting := mk(&armcompute.VirtualMachineProperties{ProvisioningState: to.Ptr("Deleting")})
	if got := a.toVMInstance(deleting); got.Running {
		t.Error("deleting VM reported Running")
	}
	// No instance view, no Deleting -> default Running.
	if got := a.toVMInstance(mk(&armcompute.VirtualMachineProperties{ProvisioningState: to.Ptr("Succeeded")})); !got.Running {
		t.Error("succeeded VM (no instance view) should default Running")
	}
}

// capacityFromSKU must round MemoryGB→MiB, not truncate: 3.5 GB can parse as
// 3.4999… and a bare int64() would floor it to 3583 MiB.
func TestCapacityFromSKU_RoundsMemory(t *testing.T) {
	caps := []*armcompute.ResourceSKUCapabilities{
		{Name: to.Ptr("vCPUs"), Value: to.Ptr("2")},
		{Name: to.Ptr("MemoryGB"), Value: to.Ptr("3.5")},
	}
	got, ok := capacityFromSKU(caps)
	if !ok {
		t.Fatal("capacityFromSKU returned ok=false")
	}
	if got.VCPU != 2 {
		t.Errorf("VCPU = %d, want 2", got.VCPU)
	}
	if got.MemMiB != 3584 {
		t.Errorf("MemMiB = %d, want 3584", got.MemMiB)
	}
}

// The real backend's Create idempotency rests on vmName: a retried CreateVM with
// the same IdempotencyToken must derive the same VM name, so ARM's
// CreateOrUpdate upserts the same VM instead of provisioning a duplicate. (The
// fake models this with a token map; this exercises the real keying directly.)
func TestVMName_DeterministicIdempotencyKey(t *testing.T) {
	if a, b := vmName("m1", "op-123"), vmName("m1", "op-123"); a != b {
		t.Errorf("same (machineID, token) gave different names: %q vs %q", a, b)
	}
	// The token is the dedup key: the same token collapses to the same name even
	// if the machine id differs.
	if a, b := vmName("m1", "op-123"), vmName("m2", "op-123"); a != b {
		t.Errorf("same token, different machineID gave different names: %q vs %q", a, b)
	}
	// Distinct tokens must map to distinct VMs.
	if a, b := vmName("m1", "op-123"), vmName("m1", "op-456"); a == b {
		t.Errorf("different tokens gave the same name: %q", a)
	}
	// With no token, the name is still deterministic from the machine id.
	id := "azure-eastus/Spot/Standard_F8s_v2/eastus-1/000"
	if a, b := vmName(id, ""), vmName(id, ""); a != b {
		t.Errorf("no-token name not deterministic: %q vs %q", a, b)
	}
}

// vmName must always yield a syntactically valid Azure VM name (non-empty,
// ≤ 64 chars, alphanumeric + hyphen, no leading/trailing hyphen) regardless of
// the punctuation or length of the seed.
func TestVMName_ValidAzureName(t *testing.T) {
	for _, in := range []string{
		"azure-eastus/Spot/Standard_F8s_v2/eastus-1/000",
		strings.Repeat("x", 200),
		"op-with-dashes_and/slashes",
		"",
	} {
		name := vmName(in, in)
		if len(name) == 0 || len(name) > 64 {
			t.Errorf("vmName(%q) length %d out of [1,64]", in, len(name))
		}
		if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
			t.Errorf("vmName(%q) = %q has a leading/trailing hyphen", in, name)
		}
		for _, r := range name {
			valid := r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !valid {
				t.Errorf("vmName(%q) = %q contains invalid rune %q", in, name, r)
			}
		}
	}
}
