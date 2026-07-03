package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// VHS is the object that controls the setup.
type VHS struct {
	Options      *Options
	Errors       []error
	Page         *rod.Page
	browser      *rod.Browser
	TextCanvas   *rod.Element
	CursorCanvas *rod.Element
	mutex        *sync.Mutex
	started      bool
	recording    bool
	tty          *exec.Cmd
	totalFrames  int
	close        func() error
	svgFrames    []SVGFrame
	// playbackSpeed is the current playback speed, stored atomically so it can
	// be updated from the evaluator goroutine while Record() reads it.
	// The float64 value is stored as its IEEE 754 bit pattern in a uint64.
	playbackSpeedBits uint64
	frameInfos        []FrameInfo
}

// FrameInfo records speed at each frame for post-processing overlays.
type FrameInfo struct {
	Speed float64
}

// Options is the set of options for the setup.
type Options struct {
	Shell               Shell
	FontFamily          string
	FontSize            int
	LetterSpacing       float64
	LineHeight          float64
	TypingSpeed         time.Duration
	TypingSpeedVariable TypingSpeedVariableOptions
	Theme               Theme
	Test                TestOptions
	Video               VideoOptions
	LoopOffset          float64
	WaitTimeout         time.Duration
	WaitPattern         *regexp.Regexp
	CursorBlink         bool
	SpeedCursor         bool
	SpeedOverlay        string
	Screenshot          ScreenshotOptions
	Style               StyleOptions
	SVG                 SVGOptions
	DebugConsole        bool // Enable browser console logging
}

// SVGOptions contains SVG-specific configuration options.
type SVGOptions struct {
	OptimizeSize bool
}

type TypingSpeedVariableOptions struct {
	MinTypingSpeed time.Duration
	MaxTypingSpeed time.Duration
}

const (
	defaultFontSize      = 22
	defaultTypingSpeed   = 50 * time.Millisecond
	defaultLineHeight    = 1.0
	defaultLetterSpacing = 1.0
	fontsSeparator       = ","
	defaultCursorBlink   = true
	defaultWaitTimeout   = 15 * time.Second
)

var defaultWaitPattern = regexp.MustCompile(">$")

var defaultFontFamily = withSymbolsFallback(strings.Join([]string{
	"JetBrains Mono",
	"DejaVu Sans Mono",
	"Menlo",
	"Bitstream Vera Sans Mono",
	"Inconsolata",
	"Roboto Mono",
	"Hack",
	"Consolas",
	"ui-monospace",
	"monospace",
}, fontsSeparator))

var symbolsFallback = []string{
	"Apple Symbols",
}

func withSymbolsFallback(font string) string {
	return font + fontsSeparator + strings.Join(symbolsFallback, fontsSeparator)
}

// DefaultSVGOptions returns the default SVG options.
func DefaultSVGOptions() SVGOptions {
	return SVGOptions{
		OptimizeSize: true, // Default to optimized SVG output
	}
}

// DefaultVHSOptions returns the default set of options to use for the setup function.
func DefaultVHSOptions() Options {
	style := DefaultStyleOptions()
	video := DefaultVideoOptions()
	video.Style = style
	screenshot := NewScreenshotOptions(video.Input, style)

	return Options{
		FontFamily:    defaultFontFamily,
		FontSize:      defaultFontSize,
		LetterSpacing: defaultLetterSpacing,
		LineHeight:    defaultLineHeight,
		TypingSpeed:   defaultTypingSpeed,
		TypingSpeedVariable: TypingSpeedVariableOptions{
			MinTypingSpeed: defaultTypingSpeed,
			MaxTypingSpeed: defaultTypingSpeed,
		},
		Shell:       Shells[defaultShell],
		Theme:       DefaultTheme,
		CursorBlink: defaultCursorBlink,
		Video:       video,
		Screenshot:  screenshot,
		WaitTimeout: defaultWaitTimeout,
		WaitPattern: defaultWaitPattern,
		SVG:         DefaultSVGOptions(),
	}
}

// New sets up ttyd and go-rod for recording frames.
func New() VHS {
	mu := &sync.Mutex{}
	opts := DefaultVHSOptions()
	v := VHS{
		Options:   &opts,
		recording: true,
		mutex:     mu,
	}
	atomic.StoreUint64(&v.playbackSpeedBits, math.Float64bits(defaultPlaybackSpeed))
	return v
}

// loadPlaybackSpeed returns the current playback speed atomically.
func (vhs *VHS) loadPlaybackSpeed() float64 {
	return math.Float64frombits(atomic.LoadUint64(&vhs.playbackSpeedBits))
}

// storePlaybackSpeed sets the current playback speed atomically.
func (vhs *VHS) storePlaybackSpeed(speed float64) {
	atomic.StoreUint64(&vhs.playbackSpeedBits, math.Float64bits(speed))
}

// Start starts ttyd, browser and everything else needed to create the gif.
func (vhs *VHS) Start() error {
	vhs.mutex.Lock()
	defer vhs.mutex.Unlock()

	if vhs.started {
		return fmt.Errorf("vhs is already started")
	}

	port := randomPort()
	vhs.tty = buildTtyCmd(port, vhs.Options.Shell)
	if err := vhs.tty.Start(); err != nil {
		return fmt.Errorf("could not start tty: %w", err)
	}

	path, _ := launcher.LookPath()
	enableNoSandbox := os.Getenv("VHS_NO_SANDBOX") != ""
	u, err := launcher.New().Leakless(false).Bin(path).NoSandbox(enableNoSandbox).Launch()
	if err != nil {
		return fmt.Errorf("could not launch browser: %w", err)
	}
	browser := rod.New().ControlURL(u).MustConnect()
	page, err := browser.Page(proto.TargetCreateTarget{URL: fmt.Sprintf("http://localhost:%d", port)})
	if err != nil {
		return fmt.Errorf("could not open ttyd: %w", err)
	}

	// Enable console logging if debug is enabled
	if vhs.Options.DebugConsole {
		go page.EachEvent(func(e *proto.RuntimeConsoleAPICalled) {
			for _, arg := range e.Args {
				log.Printf("[Browser Console] %s", arg.Value)
			}
		})()
	}

	vhs.browser = browser
	vhs.Page = page
	vhs.close = vhs.browser.Close
	vhs.started = true
	return nil
}

// Setup sets up the VHS instance and performs the necessary actions to reflect
// the options that are default and set by the user.
func (vhs *VHS) Setup() {
	// Set Viewport to the correct size, accounting for the padding that will be
	// added during the render.
	padding := vhs.Options.Video.Style.Padding
	margin := 0
	if vhs.Options.Video.Style.MarginFill != "" {
		margin = vhs.Options.Video.Style.Margin
	}
	bar := 0
	if vhs.Options.Video.Style.WindowBar != "" {
		bar = vhs.Options.Video.Style.WindowBarSize
	}
	width := vhs.Options.Video.Style.Width - double(padding) - double(margin)
	height := vhs.Options.Video.Style.Height - double(padding) - double(margin) - bar
	vhs.Page = vhs.Page.MustSetViewport(width, height, 0, false)

	// Find xterm.js canvases for the text and cursor layer for recording.
	vhs.TextCanvas, _ = vhs.Page.Element("canvas.xterm-text-layer")
	vhs.CursorCanvas, _ = vhs.Page.Element("canvas.xterm-cursor-layer")

	// Apply options to the terminal
	// By this point the setting commands have been executed, so the `opts` struct is up to date.
	vhs.Page.MustEval(fmt.Sprintf("() => { term.options = { fontSize: %d, fontFamily: '%s', letterSpacing: %f, lineHeight: %f, theme: %s, cursorBlink: %t } }",
		vhs.Options.FontSize, vhs.Options.FontFamily, vhs.Options.LetterSpacing,
		vhs.Options.LineHeight, vhs.Options.Theme.String(), vhs.Options.CursorBlink))

	// Fit the terminal into the window
	vhs.Page.MustEval("term.fit")

	_ = os.RemoveAll(vhs.Options.Video.Input)
	_ = os.MkdirAll(vhs.Options.Video.Input, 0o750)
}

const cleanupWaitTime = 100 * time.Millisecond

// Terminate cleans up a VHS instance and terminates the go-rod browser and ttyd
// processes.
//
//nolint:wrapcheck
func (vhs *VHS) terminate() error {
	// Give some time for any commands executed (such as `rm`) to finish.
	//
	// If a user runs a long running command, they must sleep for the required time
	// to finish.
	time.Sleep(cleanupWaitTime)

	// Tear down the processes we started.
	vhs.browser.MustClose()
	return vhs.tty.Process.Kill()
}

// Cleanup individual frames.
//
//nolint:wrapcheck
func (vhs *VHS) Cleanup() error {
	err := os.RemoveAll(vhs.Options.Video.Input)
	if err != nil {
		return err
	}
	return os.RemoveAll(vhs.Options.Screenshot.input)
}

// Render starts rendering the individual frames into a video.
func (vhs *VHS) Render() error {
	// Apply speed overlays before loop offset changes frame numbers.
	if err := vhs.ApplySpeedOverlays(); err != nil {
		return err
	}

	// Apply Loop Offset by modifying frame sequence
	if err := vhs.ApplyLoopOffset(); err != nil {
		return err
	}

	// Ensure the font family and size are set in the style
	if vhs.Options.Video.Style != nil {
		vhs.Options.Video.Style.FontFamily = vhs.Options.FontFamily
		vhs.Options.Video.Style.FontSize = vhs.Options.FontSize
	}

	// Generate the video(s) with the frames.
	var cmds []*exec.Cmd //nolint:prealloc
	cmds = append(cmds, MakeGIF(vhs.Options.Video))
	cmds = append(cmds, MakeMP4(vhs.Options.Video))
	cmds = append(cmds, MakeWebM(vhs.Options.Video))
	cmds = append(cmds, MakeScreenshots(vhs.Options.Screenshot)...)

	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Println(string(out))
		}
	}

	// Generate SVG if requested
	if err := MakeSVG(vhs); err != nil {
		return fmt.Errorf("failed to generate SVG: %w", err)
	}

	return nil
}

// ApplySpeedOverlays post-processes frame PNGs to draw speed indicators.
//   - SpeedCursor: draws ">> Nx" text on cursor frames where speed > 1.0,
//     positioned at the cursor's pixel location (detected by non-transparent pixels).
//   - SpeedOverlay: draws "Nx" text in the specified corner of text frames
//     wherever speed != 1.0.
func (vhs *VHS) ApplySpeedOverlays() error {
	if !vhs.Options.SpeedCursor && vhs.Options.SpeedOverlay == "" {
		return nil
	}
	for i, info := range vhs.frameInfos {
		frameNum := i + vhs.Options.Video.StartingFrame
		cursorSpeedText := formatSpeed(info.Speed)
		overlaySpeedText := formatSpeedOverlay(info.Speed)
		if vhs.Options.SpeedCursor && info.Speed > 1.0 {
			path := filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(cursorFrameFormat, frameNum))
			if err := applyCursorSpeedOverlay(path, cursorSpeedText); err != nil {
				return err
			}
		}
		if vhs.Options.SpeedOverlay != "" && info.Speed != 1.0 {
			path := filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(textFrameFormat, frameNum))
			if err := applyCornerSpeedOverlay(path, overlaySpeedText, vhs.Options.SpeedOverlay); err != nil {
				return err
			}
		}
	}
	return nil
}

// ApplyLoopOffset by modifying frame sequence.
func (vhs *VHS) ApplyLoopOffset() error {
	if vhs.totalFrames <= 0 {
		return errors.New("no frames")
	}

	loopOffsetPercentage := vhs.Options.LoopOffset

	// Calculate # of frames to offset from LoopOffset percentage
	loopOffsetFrames := int(math.Ceil(loopOffsetPercentage / 100.0 * float64(vhs.totalFrames)))

	// Take care of overflow and keep track of exact offsetPercentage
	loopOffsetFrames = loopOffsetFrames % vhs.totalFrames

	// No operation if nothing to offset
	if loopOffsetFrames <= 0 {
		return nil
	}

	// Move all frames in [offsetStart, offsetEnd] to end of frame sequence
	offsetStart := vhs.Options.Video.StartingFrame
	offsetEnd := loopOffsetFrames

	// New starting frame will be the next frame after offsetEnd
	vhs.Options.Video.StartingFrame = offsetEnd + 1

	// Rename all text and cursor frame files in the range concurrently
	errCh := make(chan error)
	doneCh := make(chan bool)
	var wg sync.WaitGroup

	for counter := offsetStart; counter <= offsetEnd; counter++ {
		wg.Add(1)
		go func(frameNum int) {
			defer wg.Done()
			offsetFrameNum := frameNum + vhs.totalFrames
			if err := os.Rename(
				filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(cursorFrameFormat, frameNum)),
				filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(cursorFrameFormat, offsetFrameNum)),
			); err != nil {
				errCh <- fmt.Errorf("error applying offset to cursor frame: %w", err)
			}
		}(counter)

		wg.Add(1)
		go func(frameNum int) {
			defer wg.Done()
			offsetFrameNum := frameNum + vhs.totalFrames
			if err := os.Rename(
				filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(textFrameFormat, frameNum)),
				filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(textFrameFormat, offsetFrameNum)),
			); err != nil {
				errCh <- fmt.Errorf("error applying offset to text frame: %w", err)
			}
		}(counter)
	}

	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		return nil
	case err := <-errCh:
		// Bail out in case of an error while renaming
		return err
	}
}

const quality = 1.0

// Record begins the goroutine which captures images from the xterm.js canvases.
func (vhs *VHS) Record(ctx context.Context) <-chan error {
	ch := make(chan error)
	interval := time.Second / time.Duration(vhs.Options.Video.Framerate)

	//nolint: mnd
	go func() {
		counter := 0
		start := time.Now()
		// accumulator drives per-section variable playback speed.
		// On each capture tick we add (1 / currentSpeed) to the accumulator.
		// A frame file is written for every whole unit that accumulates:
		//   speed > 1  → fewer frames written → section plays faster
		//   speed < 1  → more frames written  → section plays slower
		//   speed = 1  → one frame per tick (unchanged behaviour)
		accumulator := 0.0
		for {
			select {
			case <-ctx.Done():
				_ = vhs.terminate()

				// Save total # of frames for offset calculation
				vhs.totalFrames = counter

				// Signal caller that we're done recording.
				close(ch)
				return

			case <-time.After(interval - time.Since(start)):
				// record last attempt
				start = time.Now()

				if !vhs.recording {
					continue
				}
				if vhs.Page == nil {
					continue
				}

				cursor, cursorErr := vhs.CursorCanvas.CanvasToImage("image/png", quality)
				text, textErr := vhs.TextCanvas.CanvasToImage("image/png", quality)
				if textErr != nil || cursorErr != nil {
					ch <- fmt.Errorf("error: %v, %v", textErr, cursorErr)
					continue
				}

				speed := vhs.loadPlaybackSpeed()
				accumulator += 1.0 / speed

				writeErr := false
				for accumulator >= 1.0 {
					accumulator -= 1.0
					counter++
					vhs.frameInfos = append(vhs.frameInfos, FrameInfo{Speed: speed})

					if err := os.WriteFile(
						filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(cursorFrameFormat, counter)),
						cursor,
						0o600,
					); err != nil {
						ch <- fmt.Errorf("error writing cursor frame: %w", err)
						writeErr = true
						break
					}
					if err := os.WriteFile(
						filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(textFrameFormat, counter)),
						text,
						0o600,
					); err != nil {
						ch <- fmt.Errorf("error writing text frame: %w", err)
						writeErr = true
						break
					}

					// Capture SVG frame data if SVG output is requested
					if vhs.Options.Video.Output.SVG != "" {
						svgFrame, err := CaptureSVGFrame(vhs.Page, counter, vhs.Options.Video.Framerate)
						if err != nil {
							log.Printf("Error capturing SVG frame %d: %v", counter, err)
						} else if svgFrame != nil {
							vhs.svgFrames = append(vhs.svgFrames, *svgFrame)
						}
					}

					// Capture current frame and disable frame capturing.
					// Only trigger the screenshot once per capture event.
					if vhs.Options.Screenshot.frameCapture {
						vhs.Options.Screenshot.makeScreenshot(counter)
					}
				}
				if writeErr {
					continue
				}
			}
		}
	}()

	return ch
}

// ResumeRecording indicates to VHS that the recording should be resumed.
func (vhs *VHS) ResumeRecording() {
	vhs.mutex.Lock()
	defer vhs.mutex.Unlock()

	vhs.recording = true
}

// PauseRecording indicates to VHS that the recording should be paused.
func (vhs *VHS) PauseRecording() {
	vhs.mutex.Lock()
	defer vhs.mutex.Unlock()

	vhs.recording = false
}

// ScreenshotNextFrame indicates to VHS that screenshot of next frame must be taken.
func (vhs *VHS) ScreenshotNextFrame(path string) {
	vhs.mutex.Lock()
	defer vhs.mutex.Unlock()

	vhs.Options.Screenshot.enableFrameCapture(path)
}
