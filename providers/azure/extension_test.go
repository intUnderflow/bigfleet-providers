package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
)

// extRecorderTransport is a stub azcore Transporter that answers VM + extension
// ARM calls with terminal (Succeeded) responses and records the extension
// resource names that get PUT, so a test can assert the handler-uniqueness
// invariant (one CustomScript extension reused across Configure + Drain).
type extRecorderTransport struct {
	mu       sync.Mutex
	extNames []string
}

func (t *extRecorderTransport) Do(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	body := `{"properties":{"provisioningState":"Succeeded"},"tags":{}}`
	if strings.Contains(path, "/extensions/") {
		if req.Method == http.MethodPut {
			name := path[strings.LastIndex(path, "/")+1:]
			t.mu.Lock()
			t.extNames = append(t.extNames, name)
			t.mu.Unlock()
		}
		body = `{"properties":{"provisioningState":"Succeeded"}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

type fakeTokenCred struct{}

func (fakeTokenCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// Configure followed by Drain must reuse a single CustomScript extension on the
// VM. Azure permits only one extension per handler type per VM, so installing a
// second differently-named CustomScript extension would be rejected and break the
// Drain transition. This guards against reintroducing distinct extension names
// (the in-memory fake cannot model that constraint).
func TestExtension_ConfigureThenDrainReusesOneName(t *testing.T) {
	tr := &extRecorderTransport{}
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: tr}}
	exts, err := armcompute.NewVirtualMachineExtensionsClient("sub", fakeTokenCred{}, opts)
	if err != nil {
		t.Fatalf("ext client: %v", err)
	}
	vms, err := armcompute.NewVirtualMachinesClient("sub", fakeTokenCred{}, opts)
	if err != nil {
		t.Fatalf("vm client: %v", err)
	}
	a := &azureReal{
		cfg:    azureRealConfig{ResourceGroup: "rg", Location: "eastus"},
		exts:   exts,
		vms:    vms,
		logger: quietLogger(),
	}
	ctx := context.Background()
	host := vmInstance{ResourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/bf-x"}

	if err := a.ApplyBootstrap(ctx, host, "cluster-1", []byte("join-blob")); err != nil {
		t.Fatalf("ApplyBootstrap: %v", err)
	}
	if err := a.DrainNode(ctx, host, 30); err != nil {
		t.Fatalf("DrainNode: %v", err)
	}

	if len(tr.extNames) != 2 {
		t.Fatalf("expected 2 extension PUTs (configure + drain), got %d: %v", len(tr.extNames), tr.extNames)
	}
	for _, n := range tr.extNames {
		if n != bigfleetHookExtension {
			t.Errorf("extension PUT to %q, want the single %q (a second handler name would be rejected by Azure)", n, bigfleetHookExtension)
		}
	}
}
