package dhcp

import (
	"context"
	"testing"
)

// Reservations name the vendor of the MAC (IEEE registries) and flag
// locally administered (randomised) addresses, which have no vendor.
func TestStaticVendor(t *testing.T) {
	ts := newTestSvc(t, nil)
	ctx := context.Background()
	st, err := ts.CreateStatic(ctx, StaticInput{MAC: "b8:27:eb:00:00:01", IP: "192.168.1.62"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Vendor != "Raspberry Pi Foundation" || st.MACRandomized {
		t.Errorf("vendor %q randomized %v", st.Vendor, st.MACRandomized)
	}
	st, err = ts.CreateStatic(ctx, StaticInput{MAC: "02:00:00:00:00:0d", IP: "192.168.1.63"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Vendor != "" || !st.MACRandomized {
		t.Errorf("randomised MAC: vendor %q randomized %v", st.Vendor, st.MACRandomized)
	}
	for _, s := range ts.Statics() {
		if s.MAC == "b8:27:eb:00:00:01" && s.Vendor == "" {
			t.Error("Statics lacks the vendor")
		}
	}
}
