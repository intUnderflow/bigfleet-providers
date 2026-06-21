package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
// resource names that get PUT (plus the last extension PUT body), so tests can
// assert the handler-uniqueness invariant and that the join secret travels in
// ProtectedSettings, never Settings.
type extRecorderTransport struct {
	mu          sync.Mutex
	extNames    []string
	lastExtBody []byte
}

func (t *extRecorderTransport) Do(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	body := `{"properties":{"provisioningState":"Succeeded"},"tags":{}}`
	if strings.Contains(path, "/extensions/") {
		if req.Method == http.MethodPut {
			name := path[strings.LastIndex(path, "/")+1:]
			var raw []byte
			if req.Body != nil {
				raw, _ = io.ReadAll(req.Body)
			}
			t.mu.Lock()
			t.extNames = append(t.extNames, name)
			t.lastExtBody = raw
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

// The cluster-join blob's confidentiality rests on commandToExecute travelling in
// the extension's ProtectedSettings (encrypted at rest, never returned on read),
// NOT Settings (cleartext in the ARM control plane). This decodes the actual PUT
// body and asserts that invariant — a regression that moved the command into
// Settings would leak the join secret yet otherwise pass the suite.
func TestExtension_SecretInProtectedSettingsNotSettings(t *testing.T) {
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
	a := &azureReal{cfg: azureRealConfig{ResourceGroup: "rg", Location: "eastus"}, exts: exts, vms: vms, logger: quietLogger()}

	const secret = "super-secret-join-token"
	host := vmInstance{ResourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/bf-x"}
	if err := a.ApplyBootstrap(context.Background(), host, "cluster-1", []byte(secret)); err != nil {
		t.Fatalf("ApplyBootstrap: %v", err)
	}
	if tr.lastExtBody == nil {
		t.Fatal("no extension PUT body captured")
	}

	var put struct {
		Properties struct {
			Settings          map[string]any `json:"settings"`
			ProtectedSettings map[string]any `json:"protectedSettings"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tr.lastExtBody, &put); err != nil {
		t.Fatalf("decode extension body: %v", err)
	}

	cmd, _ := put.Properties.ProtectedSettings["commandToExecute"].(string)
	if cmd == "" {
		t.Fatal("commandToExecute missing from protectedSettings")
	}
	if _, ok := put.Properties.Settings["commandToExecute"]; ok {
		t.Error("commandToExecute present in cleartext Settings — must be in ProtectedSettings only")
	}
	// The base64 join blob must live only inside protectedSettings.
	b64 := base64.StdEncoding.EncodeToString([]byte(secret))
	if !strings.Contains(cmd, b64) {
		t.Error("protected commandToExecute does not carry the bootstrap blob")
	}
	if settingsJSON, _ := json.Marshal(put.Properties.Settings); strings.Contains(string(settingsJSON), b64) {
		t.Error("bootstrap blob leaked into cleartext Settings")
	}
}
