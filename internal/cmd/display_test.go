package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/studio-ch/cloudconsole-cli/internal/exitcode"
)

const displayInstance = "33333333-3333-4333-8333-333333333333"

const (
	screenshotRoute = "GET /v1/xcloud/instances/" + displayInstance + "/display/screenshot"
	inputRoute      = "POST /v1/xcloud/instances/" + displayInstance + "/display/input"
	inputOK         = `{"actions":1,"displayWidth":2560,"displayHeight":1600}`
)

// fakePNG is a PNG header announcing the given size. The CLI only reads
// the header, so the image data itself is left out.
func fakePNG(width, height byte) string {
	return "\x89PNG\r\n\x1a\n" + "\x00\x00\x00\x0dIHDR" +
		"\x00\x00\x00" + string([]byte{width}) + "\x00\x00\x00" + string([]byte{height}) + "rest"
}

func screenshotHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, map[string]route{screenshotRoute: {200, fakePNG(128, 80)}})
	h.headers = map[string]map[string]string{screenshotRoute: {
		"Content-Type": "image/png", "X-Display-Width": "256", "X-Display-Height": "160",
	}}
	return h
}

// lastInput returns the actions of the most recent input request.
func lastInput(t *testing.T, h *harness) (actions []map[string]any, body map[string]any) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.requests) - 1; i >= 0; i-- {
		if h.requests[i].Method == "POST" && strings.HasSuffix(h.requests[i].Path, "/display/input") {
			if err := json.Unmarshal([]byte(h.requests[i].Body), &body); err != nil {
				t.Fatalf("input body is not JSON: %v", err)
			}
			for _, a := range body["actions"].([]any) {
				actions = append(actions, a.(map[string]any))
			}
			return actions, body
		}
	}
	t.Fatal("no input request reached the API")
	return nil, nil
}

func TestDisplayScreenshotWritesFileAndReportsSizes(t *testing.T) {
	h := screenshotHarness(t)
	file := filepath.Join(t.TempDir(), "screen.png")
	stdout, _, code := h.run(t, "instance", "display", "screenshot", displayInstance,
		"--scale", "0.5", "--file", file, "-o", "json")
	if code != exitcode.OK {
		t.Fatalf("exit = %d, want 0", code)
	}
	written, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != fakePNG(128, 80) {
		t.Error("the file does not hold the bytes the API returned")
	}
	if q := h.seen("GET", "/v1/xcloud/instances/"+displayInstance+"/display/screenshot").Query; q != "scale=0.5" {
		t.Errorf("query = %q, want scale=0.5", q)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	want := map[string]any{"file": file, "width": 128.0, "height": 80.0, "displayWidth": 256.0, "displayHeight": 160.0}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// --file - must put the PNG and nothing else on stdout, or a redirect
// into a file produces a corrupt image.
func TestDisplayScreenshotToStdoutIsOnlyThePNG(t *testing.T) {
	h := screenshotHarness(t)
	stdout, stderr, code := h.run(t, "instance", "display", "screenshot", displayInstance, "--file", "-")
	if code != exitcode.OK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != fakePNG(128, 80) {
		t.Errorf("stdout is not exactly the PNG: %q", stdout)
	}
	if !strings.Contains(stderr, "128x80") || !strings.Contains(stderr, "256x160") {
		t.Errorf("stderr should report both sizes, got %q", stderr)
	}
	if q := h.seen("GET", "/v1/xcloud/instances/"+displayInstance+"/display/screenshot").Query; q != "" {
		t.Errorf("query = %q, want none at the default scale", q)
	}
}

func TestDisplayScreenshotRejectsANonImageAnswer(t *testing.T) {
	h := newHarness(t, map[string]route{screenshotRoute: {200, `{"oops":true}`}})
	file := filepath.Join(t.TempDir(), "screen.png")
	_, stderr, code := h.run(t, "instance", "display", "screenshot", displayInstance, "--file", file)
	if code == exitcode.OK {
		t.Fatal("a JSON answer must not be saved as a screenshot")
	}
	if !strings.Contains(stderr, "did not return a PNG") {
		t.Errorf("stderr = %q", stderr)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("no file should be written")
	}
}

func TestDisplayClickSendsOneAction(t *testing.T) {
	h := newHarness(t, map[string]route{inputRoute: {200, inputOK}})
	_, _, code := h.run(t, "instance", "display", "click", displayInstance,
		"--x", "640", "--y", "400", "--button", "right", "--count", "2", "--scale", "0.5")
	if code != exitcode.OK {
		t.Fatalf("exit = %d, want 0", code)
	}
	actions, body := lastInput(t, h)
	if len(actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(actions))
	}
	a := actions[0]
	if a["type"] != "click" || a["x"] != 640.0 || a["y"] != 400.0 || a["button"] != "right" || a["count"] != 2.0 {
		t.Errorf("unexpected action: %v", a)
	}
	if body["scale"] != 0.5 {
		t.Errorf("scale = %v, want 0.5", body["scale"])
	}
	// A repeated click must click again, so the request carries no key
	// that would let the API replay the first answer.
	if key := h.seen("POST", "/v1/xcloud/instances/"+displayInstance+"/display/input").IdempotencyKey; key != "" {
		t.Errorf("input was sent with an idempotency key %q", key)
	}
}

func TestDisplayPointerCommandsShapeTheirActions(t *testing.T) {
	cases := []struct {
		argv []string
		want map[string]any
	}{
		{[]string{"move", "--x", "1", "--y", "2"}, map[string]any{"type": "move", "x": 1.0, "y": 2.0}},
		{[]string{"drag", "--x", "1", "--y", "2", "--to-x", "30", "--to-y", "40"},
			map[string]any{"type": "drag", "x": 1.0, "y": 2.0, "toX": 30.0, "toY": 40.0}},
		{[]string{"press", "--key", "cmd+l", "--key", "enter"},
			map[string]any{"type": "press", "keys": []any{"cmd+l", "enter"}}},
	}
	for _, tc := range cases {
		t.Run(tc.argv[0], func(t *testing.T) {
			h := newHarness(t, map[string]route{inputRoute: {200, inputOK}})
			argv := append([]string{"instance", "display", tc.argv[0], displayInstance}, tc.argv[1:]...)
			if _, stderr, code := h.run(t, argv...); code != exitcode.OK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			actions, body := lastInput(t, h)
			if len(actions) != 1 {
				t.Fatalf("actions = %d, want 1", len(actions))
			}
			got, _ := json.Marshal(actions[0])
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) {
				t.Errorf("action = %s, want %s", got, want)
			}
			if _, has := body["scale"]; has {
				t.Error("scale must be left out at its default")
			}
		})
	}
}

// Long text is typed in several requests: one request may carry at most 25
// seconds of input and the client gives up after 30 seconds by default.
func TestDisplayTypeSplitsLongTextWithoutLosingAnything(t *testing.T) {
	h := newHarness(t, map[string]route{inputRoute: {200, inputOK}})
	text := strings.Repeat("grüezi ", 80) // 560 runes, multi-byte on purpose
	if _, stderr, code := h.run(t, "instance", "display", "type", displayInstance, "--text", text); code != exitcode.OK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	var typed strings.Builder
	requests := 0
	h.mu.Lock()
	for _, r := range h.requests {
		var body struct {
			Actions []struct{ Type, Text string }
		}
		if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		requests++
		for _, a := range body.Actions {
			if a.Type != "type" {
				t.Errorf("unexpected action type %q", a.Type)
			}
			if n := len([]rune(a.Text)); n > typeChunkRunes {
				t.Errorf("a request types %d characters, more than %d", n, typeChunkRunes)
			}
			typed.WriteString(a.Text)
		}
	}
	h.mu.Unlock()
	if requests != 6 {
		t.Errorf("requests = %d, want 6", requests)
	}
	if typed.String() != text {
		t.Error("the typed text differs from the input")
	}
}

// The API reads every request on its own, so the split must not change
// what is typed.
func TestTypeChunksKeepLineEndingsAndAccentsWhole(t *testing.T) {
	// CR LF across the boundary would otherwise press Enter twice.
	got := typeChunks("abc\r\ndef\rg", 4)
	if want := []string{"abc\n", "def\n", "g"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("chunks = %q, want %q", got, want)
	}
	// "e" + combining acute must stay together, even past the size.
	got = typeChunks("abce\u0301fg", 4)
	if want := []string{"abce\u0301", "fg"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("chunks = %q, want %q", got, want)
	}
	if got := typeChunks("", 4); len(got) != 0 {
		t.Errorf("empty text gave %q", got)
	}
	// Nothing is lost or reordered.
	long := strings.Repeat("grüezi\u0308 ", 50)
	if joined := strings.Join(typeChunks(long, 7), ""); joined != long {
		t.Error("joined chunks differ from the text")
	}
}

func TestDisplayCommandsRejectBadFlagsBeforeCallingTheAPI(t *testing.T) {
	cases := [][]string{
		{"click", "--x", "1", "--y", "1", "--button", "side"},
		{"click", "--x", "1", "--y", "1", "--count", "4"},
		{"click", "--x", "1", "--y", "1", "--scale", "2"},
		{"click", "--x", "1"},
		{"click", "--x", "1", "--y", "1", "--button", "middle"},
		{"screenshot", "--file", "x.png", "--scale", "0"},
		{"screenshot"},
		{"type", "--text", ""},
		{"press"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc, " "), func(t *testing.T) {
			h := newHarness(t, map[string]route{inputRoute: {200, inputOK}, screenshotRoute: {200, fakePNG(1, 1)}})
			argv := append([]string{"instance", "display", tc[0], displayInstance}, tc[1:]...)
			if _, _, code := h.run(t, argv...); code == exitcode.OK {
				t.Fatal("expected a usage error")
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.requests) != 0 {
				t.Errorf("%d request(s) reached the API", len(h.requests))
			}
		})
	}
}

func TestDisplayInputReportsTheAPIsRefusal(t *testing.T) {
	h := newHarness(t, map[string]route{inputRoute: {409,
		`{"type":"about:blank","title":"Conflict","status":409,"detail":"instance is stopped, must be running","code":"instance_not_running"}`}})
	_, stderr, code := h.run(t, "instance", "display", "press", displayInstance, "--key", "enter")
	if code == exitcode.OK {
		t.Fatal("a 409 must not exit 0")
	}
	if !strings.Contains(stderr, "instance is stopped, must be running") {
		t.Errorf("stderr = %q", stderr)
	}
}
