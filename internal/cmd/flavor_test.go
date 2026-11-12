package cmd

import (
	"net/url"
	"strings"
	"testing"
)

func TestFlavorListRegionalPlatform(t *testing.T) {
	h := newHarness(t, map[string]route{"GET /v1/xcloud/flavors": {200, `{"data":[{"slug":"mac.m1.2-6-100","label":"M1","cpuCores":2,"memoryGib":6,"diskGib":100,"platform":"linux","supportedPlatforms":["macos","linux"],"billingMode":"on_demand"}]}`}})
	out, err, code := h.run(t, "flavor", "list", "--region-id", "11111111-1111-4111-8111-111111111111", "--platform", "linux")
	if code != 0 {
		t.Fatalf("%d: %s", code, err)
	}
	req := h.seen("GET", "/v1/xcloud/flavors")
	if req == nil {
		t.Fatal("missing catalog request")
	}
	q, _ := url.ParseQuery(req.Query)
	if q.Get("platform") != "linux" || q.Get("regionId") != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("wrong query: %s", req.Query)
	}
	if !strings.Contains(out, "macos, linux") {
		t.Fatalf("missing supported systems: %s", out)
	}
}
func TestFlavorListRejectsUnknownPlatform(t *testing.T) {
	h := newHarness(t, nil)
	_, _, code := h.run(t, "flavor", "list", "--platform", "windows")
	if code == 0 || h.seen("GET", "/v1/xcloud/flavors") != nil {
		t.Fatal("invalid platform sent to server")
	}
}
