package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/studio-ch/cloudconsole-cli/internal/exitcode"
)

func TestInstanceCreateUserData(t *testing.T) {
	for _, format := range []string{"cloud-init", "ignition"} {
		t.Run(format, func(t *testing.T) {
			data := "#cloud-config\n# Grüße\nusers: []\n"
			if format == "ignition" {
				data = "{ \"ignition\": {\"version\":\"3.4.0\"}}\n"
			}
			file := filepath.Join(t.TempDir(), "user-data")
			if err := os.WriteFile(file, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, map[string]route{"POST /v1/xcloud/instances": {201, `{"id":"i-new"}`}})
			_, stderr, code := h.run(t, "instance", "create", "--name", "coreos", "--region", "11111111-1111-4111-8111-111111111111", "--image", "coreos", "--platform", "linux", "--admin-username", "core", "--user-data-file", file, "--user-data-format", format)
			if code != exitcode.OK {
				t.Fatalf("%d: %s", code, stderr)
			}
			req := h.seen("POST", "/v1/xcloud/instances")
			if req == nil {
				t.Fatal("no request")
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
				t.Fatal(err)
			}
			cfg := body["startupConfig"].(map[string]any)
			if cfg["userData"] != data || cfg["format"] != format || body["adminUsername"] != "core" {
				t.Fatal("configuration changed")
			}
			if _, ok := body["sshKeyIds"]; ok {
				t.Fatal("automatic keys mixed into custom config")
			}
		})
	}
}
func TestInstanceRejectsInvalidUserData(t *testing.T) {
	for _, tc := range []struct {
		name, data, platform, format string
		extra                        []string
	}{
		{"oversize", strings.Repeat("x", 65537), "linux", "cloud-init", nil},
		{"json", "secret-not-json", "linux", "ignition", nil},
		{"macos", "#cloud-config\nusers: []", "macos", "cloud-init", nil},
		{"keys", "#cloud-config\nusers: []", "linux", "cloud-init", []string{"--ssh-key", "key"}},
		{"format", "#cloud-config\nusers: []", "linux", "bad", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(file, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, nil)
			args := []string{"instance", "create", "--name", "test", "--region", "11111111-1111-4111-8111-111111111111", "--image", "test", "--platform", tc.platform, "--user-data-file", file, "--user-data-format", tc.format}
			_, stderr, code := h.run(t, append(args, tc.extra...)...)
			if code == exitcode.OK || h.seen("POST", "/v1/xcloud/instances") != nil {
				t.Fatal("invalid config accepted")
			}
			if strings.Contains(stderr, "secret-not-json") {
				t.Fatal("user-data leaked in error")
			}
		})
	}
}
