package tui

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

import "slices"

// region is a clickable rectangle on the rendered screen, mapped to a key.
//
// The whole mouse model is that a click inside a region is the same as
// pressing the key it names. No handler exists for anything that already has
// a keyboard binding, which is what stops the two drifting apart: the chip
// carries the literal key the handler switches on.
//
// Value carries what a click selected, for a region that picks a thing rather
// than pressing a key. A calendar cell registers "day" with the ISO date, so
// the dispatcher moves the cursor to a day it was never told about at build
// time.
type region struct {
	Row, Col      int
	Width, Height int
	Key           string
	Value         string
}

// registry is the clickable regions of one rendered frame.
//
// Rebuilt on every View, so a region that is no longer drawn cannot be
// clicked. That is also how a modal takes the whole screen: with one open,
// the cells underneath are simply never registered.
type registry struct {
	regions []region
}

// ///////////////////////////////////////////////
// Keys
// ///////////////////////////////////////////////

// keyNoop is a region that swallows a click and does nothing.
//
// A modal registers one over its own rectangle, above a full-screen region
// keyed esc, so clicking the backdrop closes it and clicking its whitespace
// does not.
const keyNoop = "noop"

// ///////////////////////////////////////////////
// Building
// ///////////////////////////////////////////////

// newRegistry returns an empty registry.
func newRegistry() *registry {
	return &registry{regions: make([]region, 0, 64)}
}

// reset clears the registry for the next frame, keeping the backing array.
func (h *registry) reset() {
	h.regions = h.regions[:0]
}

// add appends a one-row clickable region that presses a key.
func (h *registry) add(row, col, width int, key string) {
	h.addBlock(row, col, width, 1, key)
}

// addBlock appends a clickable region spanning several rows.
func (h *registry) addBlock(row, col, width, height int, key string) {
	h.addValue(row, col, width, height, key, "")
}

// addValue appends a clickable region that selects a value.
func (h *registry) addValue(row, col, width, height int, key, value string) {
	if width <= 0 || height <= 0 || key == "" {
		return
	}
	h.regions = append(h.regions, region{
		Row: row, Col: col, Width: width, Height: height, Key: key, Value: value,
	})
}

// ///////////////////////////////////////////////
// Reading
// ///////////////////////////////////////////////

// find returns the region under a point, and whether one is there.
//
// Later regions win where they overlap, so render order is click priority and
// a specific region drawn over a general one takes the click.
func (h *registry) find(x, y int) (region, bool) {
	for _, r := range slices.Backward(h.regions) {
		if y < r.Row || y >= r.Row+r.Height {
			continue
		}
		if x < r.Col || x >= r.Col+r.Width {
			continue
		}
		return r, true
	}
	return region{}, false
}
