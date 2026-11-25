package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/studio-ch/cloudconsole-cli/internal/exitcode"
)

const containerJSON = `{"id":"c-1","name":"web","status":"running","pendingAction":null,"lastError":null,` +
	`"image":"docker.io/library/nginx:1.27","cpuCores":2,"memoryGib":2,"regionSlug":"BIT1",` +
	`"networkAddress":"10.0.0.5","createdAt":"2026-11-25T10:00:00Z"}`

func TestContainerListShowsWhatMatters(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/xcloud/containers": {200, `{"data":[` + containerJSON + `,` +
			`{"id":"c-2","name":"job","status":"exited","exitCode":3,"pendingAction":null,"image":"alpine","cpuCores":1,"memoryGib":1,"regionSlug":"BIT1","createdAt":"2026-11-25T10:00:00Z"}]}`},
	})
	stdout, _, code := h.run(t, "container", "list")
	if code != exitcode.OK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"web", "running", "2 vCPU / 2 GiB", "10.0.0.5", "exited (3)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestContainerCreateBuildsTheRequest(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/regions":            {200, `{"data":[{"id":"11111111-1111-4111-8111-111111111111","slug":"BIT1","services":{"xcloud":true}}]}`},
		"POST /v1/xcloud/containers": {201, containerJSON},
	})
	_, stderr, code := h.run(t, "container", "create",
		"--name", "web", "--region", "BIT1", "--image", "docker.io/library/nginx:1.27",
		"--cpu", "1", "--memory", "2", "--storage", "5", "--network", "app",
		"--env", "LEVEL=debug", "--env", "URL=postgres://a=b",
		"--volume", "22222222-2222-4222-8222-222222222222:/data:ro",
		"--restart", "on-failure", "--elastic-ip", "new",
		"--", "sh", "-c", "echo hi")
	if code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	req := h.seen("POST", "/v1/xcloud/containers")
	if req == nil {
		t.Fatal("no create request was sent")
	}
	if req.IdempotencyKey == "" {
		t.Error("no Idempotency-Key: an interrupted create could make a second container")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["regionId"] != "11111111-1111-4111-8111-111111111111" || body["cpuCores"] != float64(1) ||
		body["memoryGib"] != float64(2) || body["writableGib"] != float64(5) || body["restartPolicy"] != "on_failure" {
		t.Errorf("unexpected body: %s", req.Body)
	}
	if _, hasFlavor := body["flavor"]; hasFlavor {
		t.Error("a custom size must not send a flavor")
	}
	env := body["env"].(map[string]any)
	if env["LEVEL"] != "debug" || env["URL"] != "postgres://a=b" {
		t.Errorf("env = %v; a value may contain '='", env)
	}
	mount := body["volumes"].([]any)[0].(map[string]any)
	if mount["path"] != "/data" || mount["readOnly"] != true {
		t.Errorf("mount = %v", mount)
	}
	if strings.Join(toStrings(body["command"]), " ") != "sh -c echo hi" {
		t.Errorf("command = %v", body["command"])
	}
	if body["elasticIp"].(map[string]any)["mode"] != "allocate" {
		t.Errorf("elasticIp = %v", body["elasticIp"])
	}
}

func toStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.(string))
	}
	return out
}

func TestContainerCreateRefusesAnAmbiguousSize(t *testing.T) {
	h := newHarness(t, map[string]route{})
	for _, argv := range [][]string{
		{"--flavor", "ctr.small", "--cpu", "2"},
		{"--cpu", "2"},
		{},
	} {
		args := append([]string{"container", "create", "--name", "x", "--region", "BIT1", "--image", "alpine", "--network", "app"}, argv...)
		if _, _, code := h.run(t, args...); code != exitcode.Usage {
			t.Errorf("%v: exit = %d, want usage error", argv, code)
		}
	}
	if h.seen("POST", "/v1/xcloud/containers") != nil {
		t.Error("a request was sent despite the usage error")
	}
}

func TestContainerLifecycleAndDelete(t *testing.T) {
	h := newHarness(t, map[string]route{
		"POST /v1/xcloud/containers/c-1/start":   {202, containerJSON},
		"POST /v1/xcloud/containers/c-1/stop":    {202, containerJSON},
		"POST /v1/xcloud/containers/c-1/restart": {202, containerJSON},
		"DELETE /v1/xcloud/containers/c-1":       {202, containerJSON},
	})
	for _, action := range []string{"start", "stop", "restart"} {
		if _, _, code := h.run(t, "container", action, "c-1"); code != exitcode.OK {
			t.Errorf("%s: exit = %d", action, code)
		}
		if h.seen("POST", "/v1/xcloud/containers/c-1/"+action) == nil {
			t.Errorf("no request for %s", action)
		}
	}
	// Without a terminal and without --yes, nothing is deleted.
	if _, _, code := h.run(t, "container", "delete", "c-1"); code == exitcode.OK {
		t.Error("delete without --yes succeeded non-interactively")
	}
	if h.seen("DELETE", "/v1/xcloud/containers/c-1") != nil {
		t.Fatal("a delete was sent without confirmation")
	}
	if _, _, code := h.run(t, "container", "delete", "c-1", "--yes"); code != exitcode.OK {
		t.Errorf("delete --yes: exit = %d", code)
	}
	if h.seen("DELETE", "/v1/xcloud/containers/c-1") == nil {
		t.Error("no delete request")
	}
}

func TestContainerLogsAndFiles(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/xcloud/containers/c-1/logs":  {200, `{"text":"listening on :8080\n","truncated":false}`},
		"GET /v1/xcloud/containers/c-1/files": {200, "file-content"},
		"PUT /v1/xcloud/containers/c-1/files": {200, `{"bytesWritten":5}`},
	})
	stdout, _, code := h.run(t, "container", "logs", "c-1")
	if code != exitcode.OK || stdout != "listening on :8080\n" {
		t.Errorf("logs: exit %d, stdout %q", code, stdout)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "got")
	if _, _, code := h.run(t, "container", "file", "get", "c-1", "/etc/hostname", "--output", out); code != exitcode.OK {
		t.Fatalf("file get: exit = %d", code)
	}
	if got, _ := os.ReadFile(out); string(got) != "file-content" {
		t.Errorf("downloaded %q", got)
	}
	if req := h.seen("GET", "/v1/xcloud/containers/c-1/files"); req == nil || !strings.Contains(req.Query, "path=%2Fetc%2Fhostname") {
		t.Errorf("download request: %+v", req)
	}

	local := filepath.Join(dir, "up")
	if err := os.WriteFile(local, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := h.run(t, "container", "file", "put", "c-1", local, "/tmp/up", "--mode", "640"); code != exitcode.OK {
		t.Fatalf("file put: exit = %d", code)
	}
	req := h.seen("PUT", "/v1/xcloud/containers/c-1/files")
	if req == nil || req.Body != "hello" || !strings.Contains(req.Query, "mode=640") {
		t.Errorf("upload request: %+v", req)
	}
}
