package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/go-fynx/reporter/internal/auth"
	"github.com/go-fynx/reporter/internal/config"
)

func runCmd(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionAndUsage(t *testing.T) {
	if code, out, _ := runCmd("version"); code != 0 || strings.TrimSpace(out) != version {
		t.Errorf("version: %d %q", code, out)
	}
	if code, _, errOut := runCmd(); code != 2 || !strings.Contains(errOut, "Usage") {
		t.Errorf("no args: %d %q", code, errOut)
	}
	if code, _, errOut := runCmd("launch"); code != 2 || !strings.Contains(errOut, `unknown command "launch"`) {
		t.Errorf("unknown command: %d %q", code, errOut)
	}
}

func TestCheckConfig(t *testing.T) {
	code, out, errOut := runCmd("check-config", "-config", "../../deploy/config.example.json")
	if code != 0 || !strings.Contains(out, "is valid") {
		t.Errorf("example config: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := runCmd("check-config", "-config", "missing.json"); code != 1 || errOut == "" {
		t.Errorf("missing file: %d %q", code, errOut)
	}
}

func TestServeFailsOnBadConfig(t *testing.T) {
	if code, _, errOut := runCmd("serve", "-config", "missing.json"); code != 1 || errOut == "" {
		t.Errorf("serve with a missing config: %d %q", code, errOut)
	}
}

func TestKeygen(t *testing.T) {
	code, out, errOut := runCmd("keygen", "-project", "booking", "-id", "booking-api")
	if code != 0 {
		t.Fatalf("keygen: %d %q", code, errOut)
	}
	key := regexp.MustCompile(`(?m)^    ([0-9a-f]{64})$`).FindStringSubmatch(out)
	if key == nil {
		t.Fatalf("no key in output:\n%s", out)
	}
	var entry config.APIKey
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &entry); err != nil {
		t.Fatalf("entry is not JSON: %v\n%s", err, out)
	}
	if entry.ID != "booking-api" || entry.Project != "booking" || entry.SHA256 != auth.Hash(key[1]) {
		t.Errorf("entry = %+v does not match key %s", entry, key[1])
	}

	if code, _, _ := runCmd("keygen"); code != 2 {
		t.Errorf("keygen without -project: code %d, want 2", code)
	}
	if code, _, _ := runCmd("keygen", "-project", "bad name"); code != 2 {
		t.Errorf("keygen with an invalid project: code %d, want 2", code)
	}
}
