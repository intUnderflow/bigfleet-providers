module github.com/intUnderflow/bigfleet-providers/providers/ovhcloud

go 1.26.4

require (
	github.com/gophercloud/gophercloud/v2 v2.12.0
	github.com/intUnderflow/bigfleet v0.0.0-20260628003822-d9e1d4c58985
	github.com/intUnderflow/bigfleet-providers v0.0.0
	github.com/prometheus/client_golang v1.23.2
	golang.org/x/crypto v0.53.0
	google.golang.org/grpc v1.80.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.67.5 // indirect
	github.com/prometheus/procfs v0.19.2 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260128011058-8636f8732409 // indirect
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af // indirect
)

// The shared providerkit library is the root module, resolved from the local
// checkout. This is an in-repo provider binary, not an importable library.
replace github.com/intUnderflow/bigfleet-providers => ../..
