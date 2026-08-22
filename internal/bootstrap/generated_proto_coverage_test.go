package bootstrap

import (
	"testing"

	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	probepb "github.com/cineko-org/contracts/v3/gen/go/cineko/probe"
)

func TestGeneratedProtoCapabilityBoundaries(t *testing.T) {
	if _, err := capabilityKeys(nil); err == nil {
		t.Fatal("nil registration accepted")
	}
	capabilities := make([]*observationpb.Capability, 0, 2)
	for _, set := range []func(*observationpb.Capability){
		func(value *observationpb.Capability) { value.SetCatalogCapture(&observationpb.CatalogCapture{}) },
		func(value *observationpb.Capability) { value.SetSeatMapCapture(&observationpb.SeatMapCapture{}) },
		func(value *observationpb.Capability) {
			value.SetSeatAvailabilityCapture(&observationpb.SeatAvailabilityCapture{})
		},
	} {
		capability := &observationpb.Capability{}
		set(capability)
		capabilities = append(capabilities, capability)
	}
	request := &probepb.RegisterRequest{}
	request.SetCapabilities(capabilities)
	keys, err := capabilityKeys(request)
	if err != nil || len(keys) != 3 {
		t.Fatalf("non-schedule capability keys = %v, %v", keys, err)
	}
	duplicate := &observationpb.Capability{}
	duplicate.SetCatalogCapture(&observationpb.CatalogCapture{})
	request.SetCapabilities([]*observationpb.Capability{duplicate, duplicate})
	if _, err := capabilityKeys(request); err == nil {
		t.Fatal("duplicate capability accepted")
	}
	if _, valid := normalizeCapabilities([]string{"unsupported"}); valid {
		t.Fatal("unsupported capability accepted")
	}
}
