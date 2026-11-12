package cmd

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/studio-ch/cloudconsole-cli/internal/api"
	"github.com/studio-ch/cloudconsole-cli/internal/output"
)

// typeChunkRunes bounds how much text one request types. The API sends
// keys at a human pace and accepts at most 25 seconds of input per request,
// so long text is typed in several requests rather than refused. An accented
// letter on macOS takes about 0.2 seconds, the slowest case; 100 of them stay
// inside that limit and inside the client's 30-second timeout.
const typeChunkRunes = 100

func newInstanceDisplayCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "display",
		Aliases: []string{"desktop"},
		Short:   "See and operate the screen of an instance (computer use)",
		Long: "See and operate the screen of a running instance.\n\n" +
			"screenshot captures the screen as a PNG; click, move, drag, type and press\n" +
			"send mouse and keyboard input. Together they let a script or an AI agent\n" +
			"use applications that have no command line: take a screenshot, decide, act,\n" +
			"and take another screenshot to see the result.\n\n" +
			"Coordinates are pixels of the screenshot, counted from its upper-left corner.\n" +
			"If you take screenshots with --scale, pass the same --scale to the input\n" +
			"commands so that a point read from the picture lands where you saw it.\n\n" +
			"Screen and input come from the instance's virtual display. This works for\n" +
			"macOS and Linux, at the login window too, and needs nothing installed in the\n" +
			"instance. Input commands return once the input is sent, before the instance\n" +
			"has reacted.",
	}
	c.AddCommand(
		newDisplayScreenshotCommand(s),
		newDisplayClickCommand(s),
		newDisplayMoveCommand(s),
		newDisplayDragCommand(s),
		newDisplayTypeCommand(s),
		newDisplayPressCommand(s),
	)
	return c
}

func displayInputColumns() []output.Column {
	return []output.Column{
		{Header: "actions", Value: func(r map[string]any) string { return output.Field(r, "actions") }},
		{Header: "display", Value: func(r map[string]any) string {
			return output.Field(r, "displayWidth") + "x" + output.Field(r, "displayHeight")
		}},
	}
}

// scaleFlag is the --scale shared by the screenshot and pointer commands.
type scaleFlag struct{ value float64 }

func (f *scaleFlag) register(c *cobra.Command) {
	c.Flags().Float64Var(&f.value, "scale", 1,
		"size of the coordinate space relative to the screen, above 0 and at most 1")
}

func (f *scaleFlag) validate() error {
	if !(f.value > 0 && f.value <= 1) {
		return &usageError{errors.New("--scale must be greater than 0 and at most 1")}
	}
	return nil
}

// sendDisplayInput posts one batch of actions and renders the API's answer.
func sendDisplayInput(cmd *cobra.Command, s *State, id string, scale float64, actions ...map[string]any) error {
	client, err := s.Client()
	if err != nil {
		return err
	}
	w, err := s.Writer()
	if err != nil {
		return err
	}
	resp, err := postDisplayInput(cmd, client, id, scale, actions)
	if err != nil {
		return err
	}
	return w.Render(resp.Body, displayInputColumns())
}

func postDisplayInput(cmd *cobra.Command, client *api.Client, id string, scale float64, actions []map[string]any) (*api.Response, error) {
	payload := map[string]any{"actions": actions}
	if scale != 1 {
		payload["scale"] = scale
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	// Deliberately no idempotency key: sending the same click twice has
	// to click twice, and a POST without one is never retried on its own.
	return client.Do(cmd.Context(), api.RequestOptions{
		Method: http.MethodPost,
		Path:   "/v1/xcloud/instances/" + url.PathEscape(id) + "/display/input",
		Body:   body,
	})
}

// pngSize reads the pixel size from a PNG's header.
func pngSize(data []byte) (width, height int, ok bool) {
	const signature = "\x89PNG\r\n\x1a\n"
	if len(data) < 24 || string(data[:8]) != signature || string(data[12:16]) != "IHDR" {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint32(data[16:20])), int(binary.BigEndian.Uint32(data[20:24])), true
}

func newDisplayScreenshotCommand(s *State) *cobra.Command {
	var (
		file  string
		scale scaleFlag
	)
	c := &cobra.Command{
		Use:   "screenshot <id>",
		Short: "Save a PNG screenshot of the instance's screen",
		Long: "Save a PNG screenshot of the instance's screen.\n\n" +
			"The command reports the size of the picture. Those are the pixels the input\n" +
			"commands count in. --scale 0.5 halves the picture, which is usually plenty for\n" +
			"a vision model and far smaller; pass the same --scale when you click.\n\n" +
			"With --file - the PNG goes to standard output and nothing else is printed there.",
		Example: "  cloudconsole instance display screenshot <id> --file screen.png\n" +
			"  cloudconsole instance display screenshot <id> --scale 0.5 --file - > screen.png",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := scale.validate(); err != nil {
				return err
			}
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			query := url.Values{}
			if scale.value != 1 {
				query.Set("scale", strconv.FormatFloat(scale.value, 'f', -1, 64))
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodGet,
				Path:   "/v1/xcloud/instances/" + url.PathEscape(args[0]) + "/display/screenshot",
				Query:  query,
			})
			if err != nil {
				return err
			}
			width, height, ok := pngSize(resp.Body)
			if !ok {
				return errors.New("the API did not return a PNG image")
			}
			displayWidth, _ := strconv.Atoi(resp.Header.Get("X-Display-Width"))
			displayHeight, _ := strconv.Atoi(resp.Header.Get("X-Display-Height"))
			if displayWidth == 0 || displayHeight == 0 {
				displayWidth, displayHeight = width, height
			}

			if file == "-" {
				if _, err := s.out().Write(resp.Body); err != nil {
					return err
				}
				fmt.Fprintf(s.errOut(), "screenshot %dx%d (display %dx%d)\n", width, height, displayWidth, displayHeight)
				return nil
			}
			if err := os.WriteFile(file, resp.Body, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", file, err)
			}
			summary, err := json.Marshal(map[string]any{
				"file": file, "width": width, "height": height,
				"displayWidth": displayWidth, "displayHeight": displayHeight,
			})
			if err != nil {
				return err
			}
			return w.Render(summary, []output.Column{
				{Header: "file", Value: func(r map[string]any) string { return output.Field(r, "file") }},
				{Header: "size", Value: func(r map[string]any) string {
					return output.Field(r, "width") + "x" + output.Field(r, "height")
				}},
				{Header: "display", Value: func(r map[string]any) string {
					return output.Field(r, "displayWidth") + "x" + output.Field(r, "displayHeight")
				}},
			})
		},
	}
	c.Flags().StringVar(&file, "file", "", "write the PNG to this path, or to standard output when the value is -")
	_ = c.MarkFlagRequired("file")
	scale.register(c)
	return c
}

// pointFlags are the --x/--y every pointer command takes.
type pointFlags struct{ x, y int }

func (p *pointFlags) register(c *cobra.Command) {
	c.Flags().IntVar(&p.x, "x", 0, "horizontal position in screenshot pixels")
	c.Flags().IntVar(&p.y, "y", 0, "vertical position in screenshot pixels")
	_ = c.MarkFlagRequired("x")
	_ = c.MarkFlagRequired("y")
}

func newDisplayClickCommand(s *State) *cobra.Command {
	var (
		at     pointFlags
		button string
		count  int
		scale  scaleFlag
	)
	c := &cobra.Command{
		Use:   "click <id>",
		Short: "Click a point on the screen",
		Example: "  cloudconsole instance display click <id> --x 640 --y 400\n" +
			"  cloudconsole instance display click <id> --x 640 --y 400 --button right\n" +
			"  cloudconsole instance display click <id> --x 640 --y 400 --count 2   # double click",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := scale.validate(); err != nil {
				return err
			}
			switch button {
			case "left", "right":
			default:
				return &usageError{fmt.Errorf("--button must be left or right, not %q", button)}
			}
			if count < 1 || count > 3 {
				return &usageError{errors.New("--count must be 1, 2 or 3")}
			}
			return sendDisplayInput(cmd, s, args[0], scale.value, map[string]any{
				"type": "click", "x": at.x, "y": at.y, "button": button, "count": count,
			})
		},
	}
	at.register(c)
	c.Flags().StringVar(&button, "button", "left", "mouse button: left or right")
	c.Flags().IntVar(&count, "count", 1, "number of clicks: 2 for a double click, 3 for a triple click")
	scale.register(c)
	return c
}

func newDisplayMoveCommand(s *State) *cobra.Command {
	var (
		at    pointFlags
		scale scaleFlag
	)
	c := &cobra.Command{
		Use:   "move <id>",
		Short: "Move the pointer without clicking",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := scale.validate(); err != nil {
				return err
			}
			return sendDisplayInput(cmd, s, args[0], scale.value, map[string]any{
				"type": "move", "x": at.x, "y": at.y,
			})
		},
	}
	at.register(c)
	scale.register(c)
	return c
}

func newDisplayDragCommand(s *State) *cobra.Command {
	var (
		from     pointFlags
		toX, toY int
		scale    scaleFlag
	)
	c := &cobra.Command{
		Use:     "drag <id>",
		Short:   "Drag with the left button from one point to another",
		Example: "  cloudconsole instance display drag <id> --x 200 --y 300 --to-x 600 --to-y 300",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := scale.validate(); err != nil {
				return err
			}
			return sendDisplayInput(cmd, s, args[0], scale.value, map[string]any{
				"type": "drag", "x": from.x, "y": from.y, "toX": toX, "toY": toY,
			})
		},
	}
	from.register(c)
	c.Flags().IntVar(&toX, "to-x", 0, "horizontal position where the drag ends")
	c.Flags().IntVar(&toY, "to-y", 0, "vertical position where the drag ends")
	_ = c.MarkFlagRequired("to-x")
	_ = c.MarkFlagRequired("to-y")
	scale.register(c)
	return c
}

// typeChunks splits text into pieces of about size runes that the API can
// type one request at a time.
//
// Each request is read on its own, so a split in the wrong place would
// change the text: a line ending written as CR LF would press Enter twice
// if the two ended up in different requests, and a letter separated from
// its combining accent ("e" + U+0301) could no longer be typed as "é".
// Line endings are therefore unified first, and a piece never ends just
// before a combining mark.
func typeChunks(text string, size int) []string {
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	runes := []rune(text)
	var chunks []string
	for start := 0; start < len(runes); {
		end := min(start+size, len(runes))
		for end < len(runes) && unicode.Is(unicode.Mn, runes[end]) {
			end++
		}
		chunks = append(chunks, string(runes[start:end]))
		start = end
	}
	return chunks
}

func newDisplayTypeCommand(s *State) *cobra.Command {
	var text string
	c := &cobra.Command{
		Use:   "type <id>",
		Short: "Type text into the focused application",
		Long: "Type text into the application that has focus. Click a text field first, or use\n" +
			"a shortcut, to choose where the text goes. A newline in the text presses Enter.\n\n" +
			"The text is typed key by key on a US keyboard layout, so the instance has to\n" +
			"use that layout. On macOS, accented letters (ä, é, ñ), the euro sign and\n" +
			"typographic punctuation are typed too; on Linux, text is limited to ASCII.\n" +
			"A character that cannot be typed is refused before anything is sent.\n\n" +
			"For shortcuts and named keys use 'display press'.",
		Example: `  cloudconsole instance display type <id> --text "hello world"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if text == "" {
				return &usageError{errors.New("--text must not be empty")}
			}
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			var last *api.Response
			for _, chunk := range typeChunks(text, typeChunkRunes) {
				last, err = postDisplayInput(cmd, client, args[0], 1, []map[string]any{
					{"type": "type", "text": chunk},
				})
				if err != nil {
					return err
				}
			}
			return w.Render(last.Body, displayInputColumns())
		},
	}
	c.Flags().StringVar(&text, "text", "", "the text to type")
	_ = c.MarkFlagRequired("text")
	return c
}

func newDisplayPressCommand(s *State) *cobra.Command {
	var keys []string
	c := &cobra.Command{
		Use:   "press <id>",
		Short: "Press keys and shortcuts",
		Long: "Press keys, in the order given. A key is one character or a named key,\n" +
			"optionally preceded by modifiers joined with +, such as cmd+space or shift+tab.\n\n" +
			"Named keys: enter, tab, space, backspace, delete, escape (esc), up, down,\n" +
			"left, right, f1 to f12.\n" +
			"Modifiers:  cmd (command; the Super key on Linux), opt (option, alt),\n" +
			"            ctrl (control), shift.\n\n" +
			"The display passes on neither the mouse wheel nor Home, End, Page Up and\n" +
			"Page Down. To scroll, press space or the arrow keys, or cmd+up and cmd+down\n" +
			"on macOS, or drag a scroll bar with 'display drag'.",
		Example: "  cloudconsole instance display press <id> --key cmd+space\n" +
			"  cloudconsole instance display press <id> --key down --key down --key enter",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(keys) == 0 {
				return &usageError{errors.New("pass at least one --key")}
			}
			return sendDisplayInput(cmd, s, args[0], 1, map[string]any{"type": "press", "keys": keys})
		},
	}
	c.Flags().StringArrayVar(&keys, "key", nil, "a key such as enter or cmd+space; repeat to press several in order")
	return c
}
