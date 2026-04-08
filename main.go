//go:build js && wasm

package main

import (
	"fmt"
	"os"
	"syscall/js"
	"time"
	"unsafe"
)

type SegmentationManagerConfig struct {
	CanvasID string
}

type RLEMask struct {
	Counts string // RLE encoded mask in COCO format
	Size   [2]int // [width, height]
}

type Mask struct {
	Color   []byte  // RGBA format
	RLEMask RLEMask // RLE encoded mask in COCO format
}

type SegmentationManager struct {
	debug         bool
	imageData     js.Value
	imageDataData js.Value
	canvas        js.Value
	buffer        []byte // buffer for rendering the masks, this will be reused to avoid allocating new memory for each frame
	canvasWidth   int
	canvasHeight  int
	canvasCtx     js.Value
	masks         map[int][]Mask // frame index to masks
	zeros         []byte         // buffer of zeros for clearing the canvas
	indexLookup   []int          // lookup for mask index to object ID, this is necessary to handle the fact that the composite mask uses a single byte to represent the mask index, which limits us to 255 masks per frame, but we may have more than 255 objects in a frame, so we need to be able to look up the correct object ID for each mask index
	cleared       bool           // flag to indicate if the canvas has been cleared at least once, this is necessary to avoid trying to render masks before the canvas has been cleared at least once, which would result in rendering artifacts due to the way the composite mask works.
}

var segmentationManager struct {
	instance *SegmentationManager
} = struct {
	instance *SegmentationManager
}{
	instance: nil,
}

func NewSegmentationManager(config *SegmentationManagerConfig) *SegmentationManager {
	canvas := js.Global().Get("document").Call("getElementById", config.CanvasID)

	if canvas.IsNull() {
		fmt.Printf("Canvas element with ID '%s' not found\n", config.CanvasID)
		os.Exit(1)
	}

	ctx := canvas.Call("getContext", "2d")

	if ctx.IsNull() {
		fmt.Printf("Failed to get 2D context from canvas with ID '%s'\n", config.CanvasID)
		os.Exit(1)
	}

	// get the canvas dimensions, we will need these to create the image data and buffer for rendering the segmentations
	width := canvas.Get("width").Int()
	height := canvas.Get("height").Int()

	// create the image data we will write to
	imageData := js.Global().Get("Uint8ClampedArray").New(js.ValueOf(width * height * 4))
	imageDataData := imageData.Get("data")

	// create the buffer that will hold the segmentation data, this will be copied into the image data to render it
	manager := &SegmentationManager{
		imageData:     imageData,
		imageDataData: imageDataData,
		canvas:        canvas,
		canvasWidth:   width,
		canvasHeight:  height,
		canvasCtx:     ctx,
		masks:         make(map[int][]Mask),
		buffer:        make([]byte, width*height*4), // RGBA format
		zeros:         make([]byte, width*height*4), // buffer of zeros for clearing the canvas
		indexLookup:   make([]int, width*height),    // pre-maps the index conversion from column first to row first data for a small perf boost.
		cleared:       true,
	}

	for i := 0; i < width*height; i++ {
		manager.indexLookup[i] = convertIndex(i, width, height)
	}

	return manager
}

type SegmentationInferenceRequestParams struct {
	ObjectID uint        `json:"object_id"`
	Labels   []int       `json:"labels"`
	Points   [][]float64 `json:"points"` // [[x,y], [x,y], ...]
}

func (as *SegmentationManager) resetMasks(index int) {
	as.masks[index] = []Mask{}
}

// Performs segmentation inference for a specific frame and object, holds the result in memory for rendering
func (as *SegmentationManager) addMask(index int, height int, width int, r, g, b byte, counts string) {
	if _, ok := as.masks[index]; !ok {
		as.masks[index] = []Mask{}
	}

	color := []byte{r, g, b, 100}

	rleMask := RLEMask{
		Counts: counts,
		Size:   [2]int{height, width},
	}

	as.masks[index] = append(as.masks[index], Mask{
		Color:   color,
		RLEMask: rleMask,
	})
}

// Performs the decoding and decompression of the RLE mask data, and creates a buffer in RGBA format that can be copied directly to the canvas for rendering. This is the most computationally intensive part of the process, but it is necessary to do this in Go to take advantage of near-native performance for handling large segmentations without needing to copy them around as much.
// This function is roughly 30-40ms per object at 2560x1440 resolution. Not fast enough for realtime.
func (as *SegmentationManager) compositeMasks(index int) []byte {
	if _, ok := as.masks[index]; !ok {
		fmt.Printf("No masks found for index %d\n", index)
		return nil
	}

	masks := as.masks[index]
	composite := make([]byte, as.canvasWidth*as.canvasHeight) // RGBA format

	// Cannot parallelize this due to single web-worker process limitations.
	for i := 0; i < len(masks); i++ {
		mask := masks[i]

		now := time.Now()

		// NOTE: we can trade off against the memory supply, BUT this is capped at 4GiB and will limit video length allowed
		//       unless we have a compelling reason, we need to lean into near-native CPU performance for rendering.
		decoded := decodeCocoRLEString(mask.RLEMask.Counts)

		if as.debug {
			fmt.Printf("Decoded RLE mask for index %d in %v\n", index, time.Since(now))
		}

		now = time.Now()

		as.restoreCocoRLE(mask.RLEMask.Size, byte(i+1), decoded, &composite)

		if as.debug {
			fmt.Printf("Restored binary masks for index %d in %v\n", index, time.Since(now))
		}
	}

	return composite
}

func (as *SegmentationManager) createImageBuffer(masks []Mask, composite []byte) []byte {
	now := time.Now()

	// RGBA format
	l := len(composite)

	if as.debug {
		fmt.Printf("Initializing image buffer for index in %v\n", time.Since(now))
	}

	now = time.Now()

	for i := 0; i < l; i++ {
		index := composite[i]

		if index == 0 { // 0 means no mask, so we can skip processing for this pixel
			continue
		}

		color := masks[index-1].Color // mask index is 1-based in the composite, so we need to subtract 1 to get the correct mask
		bufferIndex := as.indexLookup[i] * 4

		copy(as.buffer[bufferIndex:bufferIndex+4], color) // copy the RGBA color values directly into the buffer for rendering (fastest)
	}

	if as.debug {
		fmt.Printf("Created image buffer for index in %v\n", time.Since(now))
	}

	return as.buffer
}

// takes the composite mask buffer and renders the colours.

var maskRLEBuffer [1024 * 1024 * 10]byte // 10 MB buffer for a mask

//export getMaskRLEBufferAddr
func getMaskRLEBufferAddr() *byte {
	return &maskRLEBuffer[0]
}

//export resetMasks
func resetMasks(frameIndex int) {
	if segmentationManager.instance == nil {
		fmt.Printf("SegmentationManager instance is not initialized\n")
		return
	}

	segmentationManager.instance.resetMasks(frameIndex)
}

//export addMask
func addMask(frameIndex int, height int, width int, r, g, b byte, pos *byte, length uint32) {
	if segmentationManager.instance == nil {
		fmt.Printf("SegmentationManager instance is not initialized\n")
		return
	}

	data := maskRLEBuffer[:length]
	counts := string(data)

	segmentationManager.instance.addMask(frameIndex, height, width, r, g, b, counts)
}

//export renderMasks
func renderMasks(index int) uint64 {
	if segmentationManager.instance == nil {
		fmt.Printf("SegmentationManager instance is not initialized\n")
		return 0
	}

	data := segmentationManager.instance.renderMasks(index)

	// we need to return a pointer to the data buffer, but we also need to ensure that the data is not garbage collected while it is being used in the JS context, so we will return the pointer as a uint64 and keep a reference to the data in the SegmentationManager instance to prevent it from being garbage collected.
	ptr := uint64(uintptr(unsafe.Pointer(&data[0])))
	size := uint64(len(data))

	// we can return the pointer and size as a single uint64 by packing them together, this is a common technique for returning pointers from Go to JS in wasm.
	return (ptr << 32) | size
}

func (as *SegmentationManager) renderMasks(index int) []byte {
	now := time.Now()
	copy(as.buffer, as.zeros)

	if _, ok := as.masks[index]; !ok && !as.cleared { // we should clear th
		if as.debug {
			fmt.Printf("Clearing for now, no masks found for index %d\n", index)
		}
		as.cleared = true
		return as.buffer
	}

	buffer := as.createImageBuffer(as.masks[index], as.compositeMasks(index))

	if as.debug {
		fmt.Printf("Finished processing masks for index %d in %v, rendering...\n", index, time.Since(now))
	}

	return buffer
}

//////////////////////////////////////////// Launch Handles ///////////////////////////////////////////////

// This program will handle rendering of segmenatations. It is expected it will compile to wasm64,
// to take advantage of the larger address space, and be able to handle large segmentations without needing to copy them around as much. It will be used by the editor to render segmentations in the viewport, and also by the SAM2 client to render segmentations for the SAM2 UI.
func main() {
	goObj := js.Global().Get("go")

	if !goObj.Truthy() {
		fmt.Println("No 'go' object found in JavaScript")
		return
	}

	// Get the argv array
	argv := goObj.Get("argv")
	if !argv.Truthy() || argv.Get("length").Int() == 0 {
		fmt.Println("No arguments provided")
		return
	}

	l := argv.Get("length").Int()
	args := make([]string, l)

	for i := 0; i < l; i++ {
		arg := argv.Index(i).String()
		args[i] = arg
	}

	segmentationManager.instance = NewSegmentationManager(&SegmentationManagerConfig{
		CanvasID: args[0],
	})

	fmt.Printf("Loaded the Segmentation Renderer - Let's a-GO!\n")

	select {}
}

// decodes the COCORLE String into it's buffer.
// not going to even pretend to understand this.
func decodeCocoRLEString(mask string) []int {
	m := 0
	p := 0

	var k int
	var x int
	var more int

	counts := []int{}

	for p < len(mask) {
		x = 0
		k = 0
		more = 1
		for more != 0 {
			c := int(mask[p]) - 48
			x |= (c & 0x1f) << (5 * k)
			more = c & 0x20
			p++
			k++
			if more == 0 && (c&0x10) != 0 {
				x |= -1 << (5 * k)
			}
		}
		if m > 2 {
			x += counts[m-2]
		}
		counts = append(counts, x)
		m++
	}

	return counts
}

func (as *SegmentationManager) restoreCocoRLE(dim [2]int, index byte, counts []int, composite *[]byte) {
	l := len(counts)

	now := time.Now()
	acc := 0

	for i := 0; i < l; i++ {
		count := counts[i]
		active := i%2 == 1 // odd indices are "on" pixels

		if active {
			for j := 0; j < count; j++ {
				(*composite)[acc+j] = index
			}
		}

		acc += count
	}

	if as.debug {
		fmt.Printf("Restored binary mask for index %d in %v\n", index, time.Since(now))
	}
}

func convertIndex(i, width, height int) int {
	return (i%height)*width + i/height
}
