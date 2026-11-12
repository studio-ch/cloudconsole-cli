package cmd

import (
	"encoding/json"
	"github.com/studio-ch/cloudconsole-cli/internal/exitcode"
	"testing"
)

func TestNetworkCreateDHCPRange(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		h := newHarness(t, map[string]route{"POST /v1/xcloud/networks": {201, `{"name":"private","cidr":"10.0.0.0/24"}`}})
		args := []string{"network", "create", "--name", "private", "--region", "11111111-1111-4111-8111-111111111111", "--cidr", "10.0.0.0/24", "--gateway", "10.0.0.1"}
		if enabled {
			args = append(args, "--dhcp-range-start", "10.0.0.100", "--dhcp-range-end", "10.0.0.200")
		}
		_, stderr, code := h.run(t, args...)
		if code != exitcode.OK {
			t.Fatalf("%d: %s", code, stderr)
		}
		req := h.seen("POST", "/v1/xcloud/networks")
		if req == nil {
			t.Fatal("no create request")
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["mode"]; ok {
			t.Fatal("unsupported mode sent")
		}
		dhcp, ok := body["dhcp"].(map[string]any)
		if enabled && (!ok || dhcp["rangeStart"] != "10.0.0.100" || dhcp["rangeEnd"] != "10.0.0.200") {
			t.Fatalf("wrong DHCP: %v", body)
		}
		if !enabled && body["dhcp"] != nil {
			t.Fatalf("disabled DHCP sent: %v", body)
		}
	}
}
func TestNetworkRejectsIncompleteConfiguration(t *testing.T) {
	for _, extra := range [][]string{{}, {"--cidr", "10.0.0.0/24"}, {"--cidr", "10.0.0.0/24", "--gateway", "10.0.0.1", "--dhcp-range-start", "10.0.0.100"}, {"--mode", "bridge"}} {
		h := newHarness(t, nil)
		args := append([]string{"network", "create", "--name", "private", "--region", "11111111-1111-4111-8111-111111111111"}, extra...)
		_, _, code := h.run(t, args...)
		if code == exitcode.OK || h.seen("POST", "/v1/xcloud/networks") != nil {
			t.Fatal("invalid network accepted")
		}
	}
}

func TestNetworkUpdateSendsPartialPatch(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		key   string
		value any
	}{
		{[]string{"--gateway", "10.0.0.2"}, "gateway", "10.0.0.2"},
		{[]string{"--disable-dhcp"}, "dhcp", nil},
	} {
		h := newHarness(t, map[string]route{"PATCH /v1/xcloud/networks/private": {200, `{"name":"private"}`}})
		args := append([]string{"network", "update", "private", "--region", "11111111-1111-4111-8111-111111111111"}, tc.args...)
		_, stderr, code := h.run(t, args...)
		if code != exitcode.OK {
			t.Fatalf("%d: %s", code, stderr)
		}
		req := h.seen("PATCH", "/v1/xcloud/networks/private")
		if req == nil {
			t.Fatal("no PATCH")
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			t.Fatal(err)
		}
		value, ok := body[tc.key]
		if !ok || len(body) != 1 || value != tc.value {
			t.Fatalf("wrong PATCH: %v", body)
		}
	}
}
