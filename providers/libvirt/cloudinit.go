package main

import (
	"bytes"
	"fmt"

	"github.com/kdomanski/iso9660"
)

// buildNoCloudISO builds a cloud-init NoCloud datasource as an ISO9660 image
// with the volume label "cidata" (the label cloud-init's NoCloud datasource
// looks for). It contains user-data (= the opaque bootstrap blob, or a generic
// pre-binding bootstrap) and a minimal meta-data carrying the instance id and
// hostname.
//
// The image is attached to the domain as a read-only CD-ROM; cloud-init consumes
// it on (re)boot. The blob is opaque — never parsed — so it is written to
// user-data verbatim.
func buildNoCloudISO(instanceID, hostname string, userData []byte) ([]byte, error) {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return nil, fmt.Errorf("cloud-init: new iso writer: %w", err)
	}
	defer func() { _ = writer.Cleanup() }()

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname)

	// NoCloud requires user-data and meta-data; an empty user-data is valid but
	// cloud-init still wants the file present.
	if userData == nil {
		userData = []byte("#cloud-config\n{}\n")
	}
	if err := writer.AddFile(bytes.NewReader(userData), "user-data"); err != nil {
		return nil, fmt.Errorf("cloud-init: add user-data: %w", err)
	}
	if err := writer.AddFile(bytes.NewReader([]byte(metaData)), "meta-data"); err != nil {
		return nil, fmt.Errorf("cloud-init: add meta-data: %w", err)
	}

	var buf bytes.Buffer
	if err := writer.WriteTo(&buf, "cidata"); err != nil {
		return nil, fmt.Errorf("cloud-init: write iso: %w", err)
	}
	return buf.Bytes(), nil
}
