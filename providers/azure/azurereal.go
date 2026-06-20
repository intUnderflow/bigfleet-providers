package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

// azureReal is the production azureClient: it wraps azure-sdk-for-go (armcompute
// + armnetwork) authenticated with azidentity.DefaultAzureCredential, and the
// public Azure Retail Prices API for Spot pricing. It models each BigFleet
// machine as one standalone Virtual Machine (one VM per machine), which maps the
// contract's per-machine create/delete and per-machine Spot eviction far more
// directly than VMSS.
//
// The four actuator methods block until their long-running operation (poller)
// completes — that is correct, because the kit calls the backend actuators on a
// background goroutine under a per-transition timeout, so the poller IS the
// async transition work, and a poller that overruns the timeout lands the
// machine in FAILED via the cancelled ctx.
type azureReal struct {
	cfg      azureRealConfig
	logger   *slog.Logger
	vms      *armcompute.VirtualMachinesClient
	exts     *armcompute.VirtualMachineExtensionsClient
	skus     *armcompute.ResourceSKUsClient
	nics     *armnetwork.InterfacesClient
	priceAPI string // base URL of the Retail Prices API (overridable in tests)
	http     *http.Client
}

// azureRealConfig is the startup configuration for the real Azure backend.
type azureRealConfig struct {
	SubscriptionID    string
	ResourceGroup     string
	Location          string
	SubnetID          string // /subscriptions/.../subnets/<name> NICs attach to
	Image             string // VM image URN (publisher:offer:sku:version) or image resource id
	AdminUsername     string
	SSHPublicKey      []byte // authorized_keys line for the admin user
	BootstrapHookPath string // path in the image that applies the delivered blob
}

const retailPricesURL = "https://prices.azure.com/api/retail/prices"

// Tag keys stamped on every managed VM so inventory is recoverable from Azure
// alone (DescribeManaged) with no persisted store.
const (
	tagManaged   = "bigfleet-managed"
	tagMachineID = "bigfleet-machine-id"
	tagCapacity  = "bigfleet-capacity"
	tagCluster   = "bigfleet-cluster"
)

func newAzureReal(ctx context.Context, cfg azureRealConfig, logger *slog.Logger) (*azureReal, error) {
	if cfg.SubscriptionID == "" {
		return nil, fmt.Errorf("azure backend: --subscription-id (or AZURE_SUBSCRIPTION_ID) is required")
	}
	if cfg.ResourceGroup == "" {
		return nil, fmt.Errorf("azure backend: --resource-group is required")
	}
	if cfg.Location == "" {
		return nil, fmt.Errorf("azure backend: --location is required")
	}
	if cfg.SubnetID == "" {
		return nil, fmt.Errorf("azure backend: --subnet-id is required (NICs attach to it)")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}
	vmClient, err := armcompute.NewVirtualMachinesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("vm client: %w", err)
	}
	extClient, err := armcompute.NewVirtualMachineExtensionsClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("vm extensions client: %w", err)
	}
	skuClient, err := armcompute.NewResourceSKUsClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("resource skus client: %w", err)
	}
	nicClient, err := armnetwork.NewInterfacesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("nic client: %w", err)
	}
	_ = ctx
	return &azureReal{
		cfg:      cfg,
		logger:   logger,
		vms:      vmClient,
		exts:     extClient,
		skus:     skuClient,
		nics:     nicClient,
		priceAPI: retailPricesURL,
		http:     &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// CreateVM provisions the NIC then the VM, polling each to completion. The VM is
// tagged with the BigFleet machine id + capacity so DescribeManaged can recover
// inventory after a restart.
func (a *azureReal) CreateVM(ctx context.Context, spec vmSpec) (vmInstance, error) {
	name := vmName(spec.MachineID, spec.IdempotencyToken)

	nicID, err := a.ensureNIC(ctx, name)
	if err != nil {
		return vmInstance{}, fmt.Errorf("create nic: %w", err)
	}

	params := a.vmParams(spec, name, nicID)
	poller, err := a.vms.BeginCreateOrUpdate(ctx, a.cfg.ResourceGroup, name, params, nil)
	if err != nil {
		// The NIC was created but the VM never will be under this name; clean it up
		// best-effort so a permanently-failing Create doesn't leak NICs. A retried
		// Create (same operation id → same name) is idempotent either way.
		a.deleteNIC(ctx, name+"-nic")
		return vmInstance{}, fmt.Errorf("begin create vm: %w", err)
	}
	res, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		a.deleteNIC(ctx, name+"-nic")
		return vmInstance{}, fmt.Errorf("create vm poll: %w", err)
	}
	vm := a.toVMInstance(res.VirtualMachine)
	if vm.ResourceID == "" {
		return vmInstance{}, fmt.Errorf("create vm %s returned no resource id", name)
	}
	return vm, nil
}

// vmParams builds the VirtualMachine spec, including Spot priority/eviction and
// the cluster-agnostic customData bootstrap.
func (a *azureReal) vmParams(spec vmSpec, name, nicID string) armcompute.VirtualMachine {
	osProfile := &armcompute.OSProfile{
		ComputerName:  to.Ptr(name),
		AdminUsername: to.Ptr(a.cfg.AdminUsername),
	}
	if len(a.cfg.SSHPublicKey) > 0 {
		osProfile.LinuxConfiguration = &armcompute.LinuxConfiguration{
			DisablePasswordAuthentication: to.Ptr(true),
			SSH: &armcompute.SSHConfiguration{
				PublicKeys: []*armcompute.SSHPublicKey{{
					Path:    to.Ptr(fmt.Sprintf("/home/%s/.ssh/authorized_keys", a.cfg.AdminUsername)),
					KeyData: to.Ptr(string(a.cfg.SSHPublicKey)),
				}},
			},
		}
	}

	props := &armcompute.VirtualMachineProperties{
		HardwareProfile: &armcompute.HardwareProfile{
			VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(spec.VMSize)),
		},
		StorageProfile: &armcompute.StorageProfile{
			ImageReference: imageReference(a.cfg.Image),
			OSDisk: &armcompute.OSDisk{
				CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
				ManagedDisk: &armcompute.ManagedDiskParameters{
					StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS),
				},
			},
		},
		OSProfile: osProfile,
		NetworkProfile: &armcompute.NetworkProfile{
			NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
				ID: to.Ptr(nicID),
				Properties: &armcompute.NetworkInterfaceReferenceProperties{
					Primary: to.Ptr(true),
				},
			}},
		},
	}
	if len(spec.BaseUserData) > 0 {
		// cloud-init consumes osProfile.customData (base64) at first boot; the
		// separate userData field is exposed via IMDS but cloud-init does not read
		// it, so the pre-binding bootstrap goes in customData.
		props.OSProfile.CustomData = to.Ptr(base64.StdEncoding.EncodeToString(spec.BaseUserData))
	}
	if spec.Spot {
		// Spot: evict by deletion, and pay up to the pay-as-you-go price (maxPrice
		// = -1) to minimise eviction on price.
		props.Priority = to.Ptr(armcompute.VirtualMachinePriorityTypesSpot)
		props.EvictionPolicy = to.Ptr(armcompute.VirtualMachineEvictionPolicyTypesDelete)
		props.BillingProfile = &armcompute.BillingProfile{MaxPrice: to.Ptr(float64(-1))}
	}

	vm := armcompute.VirtualMachine{
		Location:   to.Ptr(a.cfg.Location),
		Properties: props,
		Tags: map[string]*string{
			tagManaged:   to.Ptr("true"),
			tagMachineID: to.Ptr(spec.MachineID),
			tagCapacity:  to.Ptr(spec.Capacity),
		},
	}
	if z := zoneNumber(spec.Zone); z != "" {
		vm.Zones = []*string{to.Ptr(z)}
	}
	return vm
}

// ensureNIC creates (or returns the existing) NIC for a VM, attached to the
// configured subnet, with dynamic private IP allocation.
func (a *azureReal) ensureNIC(ctx context.Context, vmNameStr string) (string, error) {
	nicNameStr := vmNameStr + "-nic"
	poller, err := a.nics.BeginCreateOrUpdate(ctx, a.cfg.ResourceGroup, nicNameStr, armnetwork.Interface{
		Location: to.Ptr(a.cfg.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name: to.Ptr("ipconfig1"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					Subnet:                    &armnetwork.Subnet{ID: to.Ptr(a.cfg.SubnetID)},
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
				},
			}},
		},
		Tags: map[string]*string{tagManaged: to.Ptr("true")},
	}, nil)
	if err != nil {
		return "", err
	}
	res, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", err
	}
	if res.ID == nil {
		return "", fmt.Errorf("nic %s returned no id", nicNameStr)
	}
	return *res.ID, nil
}

// DeleteVM deletes the VM and its NIC. Idempotent: a missing VM/NIC is success,
// so a Delete after a Spot eviction or an out-of-band teardown never spuriously
// fails the machine.
func (a *azureReal) DeleteVM(ctx context.Context, resourceID string) error {
	name := resourceName(resourceID)
	poller, err := a.vms.BeginDelete(ctx, a.cfg.ResourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("begin delete vm %s: %w", name, err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete vm %s poll: %w", name, err)
	}
	// Best-effort NIC cleanup; the VM is what owns the slot.
	a.deleteNIC(ctx, name+"-nic")
	return nil
}

// DescribeManaged lists every BigFleet-managed VM in the resource group.
func (a *azureReal) DescribeManaged(ctx context.Context) ([]vmInstance, error) {
	pager := a.vms.NewListPager(a.cfg.ResourceGroup, nil)
	var out []vmInstance
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list vms: %w", err)
		}
		for _, vm := range page.Value {
			if vm == nil || !hasTag(vm.Tags, tagManaged, "true") {
				continue
			}
			out = append(out, a.toVMInstance(*vm))
		}
	}
	return out, nil
}

// ApplyBootstrap delivers the opaque bootstrap blob via a CustomScript extension
// that writes the blob and runs the bootstrap hook, then records the cluster
// binding as a tag. The blob is the kubelet join data — never parsed.
func (a *azureReal) ApplyBootstrap(ctx context.Context, vm vmInstance, clusterID string, bootstrap []byte) error {
	// Decode the blob to a tmpfs path, run the hook, then remove it — preserving
	// the hook's exit code so a failed bootstrap still surfaces as FAILED. The
	// blob is the cluster-join secret, so it must not linger on the host.
	script := fmt.Sprintf("base64 -d > /run/bigfleet-bootstrap <<'EOF'\n%s\nEOF\n%s /run/bigfleet-bootstrap; rc=$?; rm -f /run/bigfleet-bootstrap; exit $rc",
		base64.StdEncoding.EncodeToString(bootstrap), a.cfg.BootstrapHookPath)
	if err := a.runExtension(ctx, resourceName(vm.ResourceID), "bigfleet-configure", script); err != nil {
		return fmt.Errorf("apply bootstrap: %w", err)
	}
	return a.setClusterTag(ctx, resourceName(vm.ResourceID), clusterID)
}

// DrainNode cordons + drains the kubelet via the bootstrap-installed drain hook,
// then clears the cluster binding tag (leaving the VM running but unbound).
func (a *azureReal) DrainNode(ctx context.Context, vm vmInstance, gracePeriodSeconds int64) error {
	script := fmt.Sprintf("%s --drain --grace=%d", a.cfg.BootstrapHookPath, gracePeriodSeconds)
	if err := a.runExtension(ctx, resourceName(vm.ResourceID), "bigfleet-drain", script); err != nil {
		return fmt.Errorf("drain node: %w", err)
	}
	return a.setClusterTag(ctx, resourceName(vm.ResourceID), "")
}

// runExtension creates/updates a Linux CustomScript extension that runs the
// given inline command, polling to completion.
//
// commandToExecute goes in ProtectedSettings, never Settings: Azure stores
// extension Settings in cleartext in the ARM control plane and returns them on
// virtualMachines/extensions/read (readable by any RG Reader / activity-log
// viewer), whereas ProtectedSettings are encrypted at rest and never returned on
// read. The configure command embeds the opaque cluster-join blob, so it must
// stay confidential.
func (a *azureReal) runExtension(ctx context.Context, vmNameStr, extName, command string) error {
	poller, err := a.exts.BeginCreateOrUpdate(ctx, a.cfg.ResourceGroup, vmNameStr, extName, armcompute.VirtualMachineExtension{
		Location: to.Ptr(a.cfg.Location),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher:               to.Ptr("Microsoft.Azure.Extensions"),
			Type:                    to.Ptr("CustomScript"),
			TypeHandlerVersion:      to.Ptr("2.1"),
			AutoUpgradeMinorVersion: to.Ptr(true),
			// Force re-run on every Configure/Drain by stamping a fresh value.
			ForceUpdateTag:    to.Ptr(fmt.Sprintf("%d", time.Now().UnixNano())),
			ProtectedSettings: map[string]any{"commandToExecute": command},
		},
	}, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

// setClusterTag records (or clears, when clusterID is empty) the cluster binding
// tag on a VM. The kit owns the authoritative binding; this keeps the substrate
// recoverable.
func (a *azureReal) setClusterTag(ctx context.Context, vmNameStr, clusterID string) error {
	get, err := a.vms.Get(ctx, a.cfg.ResourceGroup, vmNameStr, nil)
	if err != nil {
		return fmt.Errorf("get vm %s: %w", vmNameStr, err)
	}
	tags := get.Tags
	if tags == nil {
		tags = map[string]*string{}
	}
	if clusterID == "" {
		delete(tags, tagCluster)
	} else {
		tags[tagCluster] = to.Ptr(clusterID)
	}
	poller, err := a.vms.BeginUpdate(ctx, a.cfg.ResourceGroup, vmNameStr, armcompute.VirtualMachineUpdate{Tags: tags}, nil)
	if err != nil {
		return fmt.Errorf("update vm tags %s: %w", vmNameStr, err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

// SpotPriceUSD queries the public Retail Prices API for the current Spot
// consumption price of a VM size in the configured region.
func (a *azureReal) SpotPriceUSD(ctx context.Context, vmSize string) (float64, error) {
	// armSkuName is the VM size with the "Standard_" prefix stripped (the meter's
	// armSkuName, e.g. "D4s_v5").
	armSku := strings.TrimPrefix(vmSize, "Standard_")
	filter := fmt.Sprintf("armRegionName eq '%s' and armSkuName eq '%s' and priceType eq 'Consumption'",
		a.cfg.Location, armSku)
	q := url.Values{}
	q.Set("currencyCode", "USD")
	q.Set("$filter", filter)
	reqURL := a.priceAPI + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("retail prices: status %d", resp.StatusCode)
	}
	var body struct {
		Items []struct {
			UnitPrice   float64 `json:"unitPrice"`
			MeterName   string  `json:"meterName"`
			ProductName string  `json:"productName"`
			SKUName     string  `json:"skuName"`
		} `json:"Items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("retail prices decode: %w", err)
	}
	best := -1.0
	for _, it := range body.Items {
		if !isSpotMeter(it.MeterName, it.SKUName, it.ProductName) {
			continue
		}
		// The Retail Prices API returns both Linux and Windows meters for the same
		// (region, SKU); Windows carries a licence surcharge, so include only the
		// Linux meter (Windows productName ends in " Windows").
		if strings.Contains(strings.ToLower(it.ProductName), "windows") {
			continue
		}
		if it.UnitPrice <= 0 {
			continue
		}
		if best < 0 || it.UnitPrice < best {
			best = it.UnitPrice
		}
	}
	if best < 0 {
		return 0, fmt.Errorf("no spot consumption meter for %s in %s", vmSize, a.cfg.Location)
	}
	return best, nil
}

// DescribeVMSizeCapacities resolves vCPU + memory for the given VM sizes from the
// Resource SKUs API (filtered to the configured location).
func (a *azureReal) DescribeVMSizeCapacities(ctx context.Context, vmSizes []string) (map[string]vmCapacity, error) {
	want := make(map[string]bool, len(vmSizes))
	for _, s := range vmSizes {
		want[s] = true
	}
	out := make(map[string]vmCapacity, len(vmSizes))
	filter := fmt.Sprintf("location eq '%s'", a.cfg.Location)
	pager := a.skus.NewListPager(&armcompute.ResourceSKUsClientListOptions{Filter: to.Ptr(filter)})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list resource skus: %w", err)
		}
		for _, sku := range page.Value {
			if sku == nil || sku.Name == nil || sku.ResourceType == nil || *sku.ResourceType != "virtualMachines" {
				continue
			}
			if !want[*sku.Name] {
				continue
			}
			cap, ok := capacityFromSKU(sku.Capabilities)
			if ok {
				out[*sku.Name] = cap
			}
		}
	}
	return out, nil
}

func (a *azureReal) toVMInstance(vm armcompute.VirtualMachine) vmInstance {
	out := vmInstance{Running: true}
	if vm.ID != nil {
		out.ResourceID = *vm.ID
	}
	if vm.Name != nil {
		out.Name = *vm.Name
	}
	out.MachineID = tagValue(vm.Tags, tagMachineID)
	out.Capacity = tagValue(vm.Tags, tagCapacity)
	out.ClusterID = tagValue(vm.Tags, tagCluster)
	if len(vm.Zones) > 0 && vm.Zones[0] != nil {
		out.Zone = a.cfg.Location + "-" + *vm.Zones[0]
	}
	if p := vm.Properties; p != nil {
		if p.HardwareProfile != nil && p.HardwareProfile.VMSize != nil {
			out.VMSize = string(*p.HardwareProfile.VMSize)
		}
		if p.Priority != nil && *p.Priority == armcompute.VirtualMachinePriorityTypesSpot {
			out.Spot = true
		}
		if p.ProvisioningState != nil && strings.EqualFold(*p.ProvisioningState, "Deleting") {
			out.Running = false
		}
	}
	return out
}

// --- helpers ---------------------------------------------------------------

// deleteNIC deletes a NIC best-effort, polling to completion. Used both on VM
// teardown and to clean up after a failed VM create. A missing NIC is success.
func (a *azureReal) deleteNIC(ctx context.Context, nicNameStr string) {
	poller, err := a.nics.BeginDelete(ctx, a.cfg.ResourceGroup, nicNameStr, nil)
	if err != nil {
		return
	}
	_, _ = poller.PollUntilDone(ctx, nil)
}

// vmName derives a valid, deterministic Azure VM name (≤ 64 chars,
// alphanumeric + hyphen) from the machine id, preferring the idempotency token
// so a retried Create maps to the same VM.
func vmName(machineID, token string) string {
	seed := token
	if seed == "" {
		seed = machineID
	}
	var b strings.Builder
	b.WriteString("bf-")
	for _, r := range seed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '/' || r == '_':
			b.WriteByte('-')
		}
	}
	name := b.String()
	if len(name) > 60 {
		name = name[len(name)-60:]
	}
	return strings.Trim(name, "-")
}

// resourceName extracts the trailing resource name from an ARM resource id.
func resourceName(resourceID string) string {
	if i := strings.LastIndex(resourceID, "/"); i >= 0 {
		return resourceID[i+1:]
	}
	return resourceID
}

// zoneNumber maps a BigFleet zone ("eastus-1") to the bare Azure zone number
// ("1"). An empty or numberless zone yields "" (no zone constraint).
func zoneNumber(zone string) string {
	if zone == "" {
		return ""
	}
	if i := strings.LastIndex(zone, "-"); i >= 0 {
		return zone[i+1:]
	}
	return zone
}

// imageReference parses a publisher:offer:sku:version URN into an ImageReference,
// or treats the value as a managed image / gallery resource id.
func imageReference(image string) *armcompute.ImageReference {
	if strings.HasPrefix(image, "/") {
		return &armcompute.ImageReference{ID: to.Ptr(image)}
	}
	parts := strings.Split(image, ":")
	if len(parts) == 4 {
		return &armcompute.ImageReference{
			Publisher: to.Ptr(parts[0]),
			Offer:     to.Ptr(parts[1]),
			SKU:       to.Ptr(parts[2]),
			Version:   to.Ptr(parts[3]),
		}
	}
	// Fall back to a sane default Ubuntu LTS image.
	return &armcompute.ImageReference{
		Publisher: to.Ptr("Canonical"),
		Offer:     to.Ptr("ubuntu-24_04-lts"),
		SKU:       to.Ptr("server"),
		Version:   to.Ptr("latest"),
	}
}

func capacityFromSKU(caps []*armcompute.ResourceSKUCapabilities) (vmCapacity, bool) {
	var out vmCapacity
	var haveCPU, haveMem bool
	for _, c := range caps {
		if c == nil || c.Name == nil || c.Value == nil {
			continue
		}
		switch *c.Name {
		case "vCPUs":
			var n int
			if _, err := fmt.Sscanf(*c.Value, "%d", &n); err == nil {
				out.VCPU = n
				haveCPU = true
			}
		case "MemoryGB":
			var g float64
			if _, err := fmt.Sscanf(*c.Value, "%g", &g); err == nil {
				out.MemMiB = int64(g * 1024)
				haveMem = true
			}
		}
	}
	return out, haveCPU && haveMem
}

func tagValue(tags map[string]*string, key string) string {
	if v, ok := tags[key]; ok && v != nil {
		return *v
	}
	return ""
}

func hasTag(tags map[string]*string, key, want string) bool {
	return tagValue(tags, key) == want
}

func isSpotMeter(meterName, skuName, productName string) bool {
	return strings.Contains(strings.ToLower(meterName), "spot") ||
		strings.Contains(strings.ToLower(skuName), "spot") ||
		strings.Contains(strings.ToLower(productName), "spot")
}

// isNotFound reports whether err is an Azure 404 (resource already gone), so a
// delete of a missing VM is treated as success. It inspects the typed
// azcore.ResponseError status code rather than matching error strings.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusNotFound
	}
	return false
}

var _ azureClient = (*azureReal)(nil)
