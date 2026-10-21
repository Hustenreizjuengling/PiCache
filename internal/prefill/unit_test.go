package prefill

import (
	"os"
	"strings"
	"testing"
)

func TestUnitFile(t *testing.T) {
	b, err := os.ReadFile("../../deploy/systemd/picache-prefill.service")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != UnitFile {
		t.Fatal("UnitFile differs from deploy/systemd/picache-prefill.service: copy the file into unit.go")
	}
	if strings.Count(UnitFile, UnitBinary) != 1 || !strings.Contains(UnitFile, "ExecStart="+UnitBinary+" prefill\n") {
		t.Fatal("ExecStart does not name UnitBinary")
	}
	for _, want := range []string{"DynamicUser=yes", "CapabilityBoundingSet=\n", "NoNewPrivileges=yes", "RuntimeDirectory=picache-prefill"} {
		if !strings.Contains(UnitFile, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
}
