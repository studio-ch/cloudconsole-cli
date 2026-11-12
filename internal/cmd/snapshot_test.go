package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/studio-ch/cloudconsole-cli/internal/exitcode"
)

const (
	snapID     = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	snapRegion = "11111111-2222-3333-4444-555555555555"
)

const snapshotListBody = `{"data":[{"id":"` + snapID + `","name":"before-upgrade","status":"ready",
	"regionId":"` + snapRegion + `","regionSlug":"ZRH1","platform":"linux","diskGib":120,
	"minCpuCores":4,"minMemoryGib":8,"sourceInstanceId":null,"sourceInstanceName":"build-01",
	"createdAt":"2026-11-01T10:00:00.000Z"}]}`

func decodeBody(t *testing.T, req *recorded) map[string]any {
	t.Helper()
	if req == nil {
		t.Fatal("request never reached the API")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		t.Fatalf("request body is not JSON: %v (%q)", err, req.Body)
	}
	return body
}

func TestSnapshotListSendsFilters(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/regions":          {200, `{"data":[{"id":"` + snapRegion + `","slug":"ZRH1","services":{"xcloud":true}}]}`},
		"GET /v1/xcloud/snapshots": {200, snapshotListBody},
	})
	stdout, stderr, code := h.run(t, "snapshot", "list", "--instance", "i-1", "--region", "ZRH1")
	if code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	req := h.seen("GET", "/v1/xcloud/snapshots")
	if req == nil {
		t.Fatal("no list request")
	}
	if req.Query != "instanceId=i-1&regionId="+snapRegion {
		t.Errorf("query = %q", req.Query)
	}
	// A snapshot whose instance is gone must say so — it can no longer be
	// restored, only seed new instances.
	if !strings.Contains(stdout, "build-01 (deleted)") {
		t.Errorf("table should flag the deleted source instance, got:\n%s", stdout)
	}
}

func TestSnapshotGetResolvesName(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/xcloud/snapshots":           {200, snapshotListBody},
		"GET /v1/xcloud/snapshots/" + snapID: {200, `{"id":"` + snapID + `","name":"before-upgrade","status":"ready"}`},
	})
	_, stderr, code := h.run(t, "snapshot", "get", "before-upgrade")
	if code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if h.seen("GET", "/v1/xcloud/snapshots/"+snapID) == nil {
		t.Error("the name was not resolved to the snapshot id")
	}
}

func TestSnapshotUnknownNameIsAUsageError(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/xcloud/snapshots": {200, snapshotListBody},
	})
	_, stderr, code := h.run(t, "snapshot", "get", "nope")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(stderr, `"nope"`) {
		t.Errorf("error should name the snapshot, got: %s", stderr)
	}
}

func TestSnapshotCreatePostsToTheInstance(t *testing.T) {
	h := newHarness(t, map[string]route{
		"POST /v1/xcloud/instances/i-1/snapshots": {201, `{"id":"` + snapID + `","status":"creating"}`},
	})
	_, stderr, code := h.run(t, "snapshot", "create", "--instance", "i-1", "--name", "pre", "--description", "before upgrade")
	if code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	body := decodeBody(t, h.seen("POST", "/v1/xcloud/instances/i-1/snapshots"))
	if body["name"] != "pre" || body["description"] != "before upgrade" {
		t.Errorf("body = %v", body)
	}
}

// Without --name the API picks '<instance> <timestamp>'; the CLI must not
// send an empty name that the API would reject.
func TestSnapshotCreateOmitsUnsetName(t *testing.T) {
	h := newHarness(t, map[string]route{
		"POST /v1/xcloud/instances/i-1/snapshots": {201, `{"id":"` + snapID + `"}`},
	})
	if _, stderr, code := h.run(t, "snapshot", "create", "--instance", "i-1"); code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	body := decodeBody(t, h.seen("POST", "/v1/xcloud/instances/i-1/snapshots"))
	if len(body) != 0 {
		t.Errorf("body = %v, want {}", body)
	}
}

func TestSnapshotUpdate(t *testing.T) {
	path := "/v1/xcloud/snapshots/" + snapID
	t.Run("rename", func(t *testing.T) {
		h := newHarness(t, map[string]route{"PATCH " + path: {200, `{"id":"` + snapID + `"}`}})
		if _, stderr, code := h.run(t, "snapshot", "rename", snapID, "--name", "baseline"); code != exitcode.OK {
			t.Fatalf("exit = %d: %s", code, stderr)
		}
		body := decodeBody(t, h.seen("PATCH", path))
		if body["name"] != "baseline" {
			t.Errorf("body = %v", body)
		}
		if _, ok := body["description"]; ok {
			t.Errorf("description sent although not given: %v", body)
		}
	})
	t.Run("clear description", func(t *testing.T) {
		h := newHarness(t, map[string]route{"PATCH " + path: {200, `{"id":"` + snapID + `"}`}})
		if _, stderr, code := h.run(t, "snapshot", "update", snapID, "--description", ""); code != exitcode.OK {
			t.Fatalf("exit = %d: %s", code, stderr)
		}
		body := decodeBody(t, h.seen("PATCH", path))
		if v, ok := body["description"]; !ok || v != nil {
			t.Errorf("an empty --description must send null, body = %v", body)
		}
	})
	t.Run("nothing to change", func(t *testing.T) {
		h := newHarness(t, map[string]route{})
		if _, _, code := h.run(t, "snapshot", "update", snapID); code != exitcode.Usage {
			t.Errorf("exit = %d, want %d", code, exitcode.Usage)
		}
	})
}

func TestSnapshotDeleteRequiresConfirmation(t *testing.T) {
	path := "/v1/xcloud/snapshots/" + snapID
	h := newHarness(t, map[string]route{"DELETE " + path: {202, `{"id":"` + snapID + `","status":"deleting"}`}})
	if _, _, code := h.run(t, "snapshot", "delete", snapID); code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if h.seen("DELETE", path) != nil {
		t.Fatal("a delete was sent without confirmation")
	}
	if _, stderr, code := h.run(t, "snapshot", "delete", snapID, "--yes"); code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if h.seen("DELETE", path) == nil {
		t.Error("no delete request with --yes")
	}
}

// scriptedServer answers GETs of one path from a sequence of responses
// (the last one repeats) and every other request with 202.
func scriptedServer(t *testing.T, path string, responses ...route) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != path {
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"` + snapID + `","status":"creating"}`))
			return
		}
		mu.Lock()
		rt := responses[min(calls, len(responses)-1)]
		calls++
		mu.Unlock()
		if rt.status == 404 {
			w.Header().Set("Content-Type", "application/problem+json")
		}
		w.WriteHeader(rt.status)
		_, _ = w.Write([]byte(rt.body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

func runAgainst(t *testing.T, srv *httptest.Server, argv ...string) (string, exitcode.Code) {
	t.Helper()
	t.Setenv("CLOUDCONSOLE_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Setenv("CLOUDCONSOLE_API_URL", srv.URL)
	t.Setenv("CLOUDCONSOLE_API_TOKEN", fakeToken)
	t.Setenv("CLOUDCONSOLE_ALLOW_INSECURE", "1")
	t.Setenv("CLOUDCONSOLE_PROFILE", "")
	t.Setenv("CLOUDCONSOLE_OUTPUT", "")
	t.Setenv("NO_COLOR", "1")

	var out, errBuf bytes.Buffer
	s := &State{stdout: &out, stderr: &errBuf}
	root := newRootCommand(s)
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(argv)
	code := exitcode.OK
	if err := root.ExecuteContext(context.Background()); err != nil {
		code = report(&errBuf, err, s)
	}
	return errBuf.String(), code
}

func TestSnapshotCreateWaitPollsUntilReady(t *testing.T) {
	srv, calls := scriptedServer(t, "/v1/xcloud/snapshots/"+snapID,
		route{200, `{"id":"` + snapID + `","status":"creating"}`},
		route{200, `{"id":"` + snapID + `","status":"ready"}`},
	)
	stderr, code := runAgainst(t, srv, "snapshot", "create", "--instance", "i-1", "--wait", "--wait-timeout", "30s")
	if code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	// Two polls by the waiter, one re-read for the rendered row.
	if calls() < 3 {
		t.Errorf("polled %d times; --wait must keep polling while the snapshot is creating", calls())
	}
}

func TestSnapshotCreateWaitFailsOnFailed(t *testing.T) {
	srv, _ := scriptedServer(t, "/v1/xcloud/snapshots/"+snapID,
		route{200, `{"id":"` + snapID + `","status":"failed","lastError":"disk busy"}`},
	)
	stderr, code := runAgainst(t, srv, "snapshot", "create", "--instance", "i-1", "--wait", "--wait-timeout", "30s")
	if code == exitcode.OK {
		t.Fatal("a failed snapshot must not exit 0")
	}
	if !strings.Contains(stderr, "disk busy") {
		t.Errorf("error should carry lastError, got: %s", stderr)
	}
}

func TestSnapshotDeleteWaitUntilGone(t *testing.T) {
	srv, calls := scriptedServer(t, "/v1/xcloud/snapshots/"+snapID,
		route{200, `{"id":"` + snapID + `","status":"deleting"}`},
		route{404, `{"type":"about:blank","title":"Not Found","status":404}`},
	)
	stderr, code := runAgainst(t, srv, "snapshot", "delete", snapID, "--yes", "--wait", "--wait-timeout", "30s")
	if code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if calls() < 2 {
		t.Errorf("polled %d times; --wait must poll until the snapshot is gone", calls())
	}
	if !strings.Contains(stderr, "done (deleted)") {
		t.Errorf("stderr = %s", stderr)
	}
}

func TestInstanceRestore(t *testing.T) {
	path := "/v1/xcloud/instances/i-1/restore"
	for _, tc := range []struct {
		flag string
		want any // nil: startAfter must be omitted
	}{{"", nil}, {"--start-after", true}, {"--no-start-after", false}} {
		t.Run("flag="+tc.flag, func(t *testing.T) {
			h := newHarness(t, map[string]route{
				"GET /v1/xcloud/snapshots": {200, snapshotListBody},
				"POST " + path:             {202, `{"id":"i-1","status":"restoring","pendingAction":"restore"}`},
			})
			argv := []string{"instance", "restore", "i-1", "--snapshot", "before-upgrade", "--yes"}
			if tc.flag != "" {
				argv = append(argv, tc.flag)
			}
			if _, stderr, code := h.run(t, argv...); code != exitcode.OK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			// A name is looked up among this instance's snapshots only.
			if list := h.seen("GET", "/v1/xcloud/snapshots"); list == nil || list.Query != "instanceId=i-1" {
				t.Errorf("name lookup = %+v, want a list filtered to the instance", list)
			}
			body := decodeBody(t, h.seen("POST", path))
			if body["snapshotId"] != snapID {
				t.Errorf("snapshotId = %v", body["snapshotId"])
			}
			got, ok := body["startAfter"]
			if tc.want == nil && ok {
				t.Errorf("startAfter sent without a flag: %v", body)
			}
			if tc.want != nil && got != tc.want {
				t.Errorf("startAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInstanceRestoreRefusesWithoutConfirmation(t *testing.T) {
	h := newHarness(t, map[string]route{"POST /v1/xcloud/instances/i-1/restore": {202, `{}`}})
	if _, _, code := h.run(t, "instance", "restore", "i-1", "--snapshot", snapID); code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if h.seen("POST", "/v1/xcloud/instances/i-1/restore") != nil {
		t.Error("a restore was sent without confirmation")
	}
}

func TestInstanceRestoreStartFlagsAreExclusive(t *testing.T) {
	h := newHarness(t, map[string]route{})
	_, _, code := h.run(t, "instance", "restore", "i-1", "--snapshot", snapID, "--yes", "--start-after", "--no-start-after")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
}

// --from-snapshot replaces --image, and region, platform and sizing
// default to the smallest instance the API accepts for that snapshot.
func TestInstanceCreateFromSnapshotDefaults(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/xcloud/snapshots":  {200, snapshotListBody},
		"POST /v1/xcloud/instances": {201, `{"id":"i-new","status":"pending","pendingAction":"create"}`},
	})
	_, stderr, code := h.run(t, "instance", "create", "--name", "copy", "--from-snapshot", "before-upgrade", "--memory", "16")
	if code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	body := decodeBody(t, h.seen("POST", "/v1/xcloud/instances"))
	if body["sourceSnapshotId"] != snapID {
		t.Errorf("sourceSnapshotId = %v", body["sourceSnapshotId"])
	}
	if _, ok := body["imageRef"]; ok {
		t.Errorf("imageRef sent together with sourceSnapshotId: %v", body)
	}
	if body["regionId"] != snapRegion || body["platform"] != "linux" {
		t.Errorf("region/platform not defaulted from the snapshot: %v", body)
	}
	if body["diskGib"] != float64(120) || body["cpuCores"] != float64(4) || body["memoryGib"] != float64(16) {
		t.Errorf("sizing = %v, want disk 120, cpu 4 and the explicit memory 16", body)
	}
}

func TestInstanceCreateImageSourceFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"instance", "create", "--name", "x", "--region", "ZRH1"},
		{"instance", "create", "--name", "x", "--region", "ZRH1", "--image", "img", "--from-snapshot", snapID},
		{"instance", "create", "--name", "x", "--image", "img"},
		{"instance", "create", "--name", "x", "--from-snapshot", snapID, "--admin-username", "root"},
	} {
		h := newHarness(t, map[string]route{})
		if _, _, code := h.run(t, argv...); code != exitcode.Usage {
			t.Errorf("%v: exit = %d, want %d", argv, code, exitcode.Usage)
		}
		if h.seen("POST", "/v1/xcloud/instances") != nil {
			t.Errorf("%v: a create was sent", argv)
		}
	}
}

func TestInstanceDeleteSnapshotsFlag(t *testing.T) {
	h := newHarness(t, map[string]route{"DELETE /v1/xcloud/instances/i-1": {202, ""}})
	if _, stderr, code := h.run(t, "instance", "delete", "i-1", "--yes", "--delete-snapshots"); code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	req := h.seen("DELETE", "/v1/xcloud/instances/i-1")
	if req == nil || req.Query != "deleteSnapshots=true" {
		t.Errorf("request = %+v, want deleteSnapshots=true", req)
	}
}
