package main

import (
	"encoding/xml"
	"fmt"
)

// BigFleet domain metadata namespace. The machine id and cluster binding are
// stored in the domain's libvirt metadata (a custom XML element) so inventory
// and bindings can be recovered from libvirt alone after a restart, without a
// persisted store.
const (
	bigfleetMetadataNS  = "https://bigfleet.dev/libvirt/v1"
	bigfleetMetadataKey = "bigfleet"
)

// bigfleetMeta is the custom metadata element attached to every managed domain.
type bigfleetMeta struct {
	XMLName   xml.Name `xml:"bigfleet:bigfleet"`
	NS        string   `xml:"xmlns:bigfleet,attr"`
	MachineID string   `xml:"machineID"`
	ClusterID string   `xml:"clusterID,omitempty"`
}

func newBigfleetMeta(machineID, clusterID string) bigfleetMeta {
	return bigfleetMeta{NS: bigfleetMetadataNS, MachineID: machineID, ClusterID: clusterID}
}

func (b bigfleetMeta) marshal() (string, error) {
	out, err := xml.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("marshal bigfleet metadata: %w", err)
	}
	return string(out), nil
}

// domainParams are the inputs to the domain XML template.
type domainParams struct {
	Name      string
	VCPUs     int
	MemoryMiB int64
	// DiskPath is the path to the overlay qcow2 the domain boots from.
	DiskPath string
	// SeedPath is the path to the cloud-init NoCloud ISO, attached read-only.
	SeedPath string
	// Network is the libvirt network the NIC attaches to.
	Network string
	// Metadata is the marshalled bigfleet metadata element.
	Metadata string
}

// renderDomainXML builds a libvirt domain definition for a QEMU/KVM VM: the
// requested vCPU/memory, a virtio overlay disk booting the base image, a virtio
// NIC on the configured network, a cloud-init NoCloud CD-ROM seed, and the qemu
// guest agent channel (used by Configure/Drain to run in-guest commands without
// a reboot). The bigfleet metadata element tags the domain for inventory
// recovery.
//
// The XML is assembled from a fixed template with only validated/derived values
// interpolated (names are domain-safe, paths come from libvirt-created volumes),
// so it is not attacker-controlled.
func renderDomainXML(p domainParams) string {
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%[1]s</name>
  <metadata>
    %[7]s
  </metadata>
  <memory unit='MiB'>%[2]d</memory>
  <currentMemory unit='MiB'>%[2]d</currentMemory>
  <vcpu placement='static'>%[3]d</vcpu>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <boot dev='hd'/>
  </os>
  <features>
    <acpi/>
    <apic/>
  </features>
  <cpu mode='host-passthrough' check='none'/>
  <clock offset='utc'/>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>restart</on_crash>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%[4]s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%[5]s'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <interface type='network'>
      <source network='%[6]s'/>
      <model type='virtio'/>
    </interface>
    <channel type='unix'>
      <source mode='bind'/>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
    <console type='pty'/>
    <serial type='pty'/>
  </devices>
</domain>`,
		p.Name, p.MemoryMiB, p.VCPUs, p.DiskPath, p.SeedPath, p.Network, p.Metadata)
}

// overlayVolumeXML builds the storage-volume definition for a copy-on-write
// overlay disk backed by the golden base image. virStorageVolCreateXML with this
// definition creates a thin qcow2 overlay, so each VM gets a writable disk
// without copying the whole base image. backingFormat must be the actual format
// of the base image (qcow2 or raw) — misdeclaring a raw base as qcow2 corrupts
// the overlay's view of it.
func overlayVolumeXML(name, basePath, backingFormat string, capacityBytes uint64) string {
	return fmt.Sprintf(`<volume type='file'>
  <name>%[1]s</name>
  <capacity unit='bytes'>%[3]d</capacity>
  <target>
    <format type='qcow2'/>
  </target>
  <backingStore>
    <path>%[2]s</path>
    <format type='%[4]s'/>
  </backingStore>
</volume>`, name, basePath, capacityBytes, backingFormat)
}

// seedVolumeXML builds the storage-volume definition for the cloud-init NoCloud
// ISO (a raw volume the ISO bytes are uploaded into).
func seedVolumeXML(name string, sizeBytes int64) string {
	return fmt.Sprintf(`<volume type='file'>
  <name>%[1]s</name>
  <capacity unit='bytes'>%[2]d</capacity>
  <target>
    <format type='raw'/>
  </target>
</volume>`, name, sizeBytes)
}
