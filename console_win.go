// +build windows

// Copyright 2015 The TCell Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License.
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tcell

import (
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

type cScreen struct {
	in    syscall.Handle
	out   syscall.Handle
	mbtns uint32 // debounce mouse buttons
	evch  chan Event
	quit  chan struct{}
	curx  int
	cury  int
	style Style
	clear bool

	w int
	h int

	oscreen consoleInfo
	ocursor cursorInfo
	oimode  uint32
	oomode  uint32
	cells   []Cell

	sync.Mutex
}

// all Windows systems are little endian
var k32 = syscall.NewLazyDLL("kernel32.dll")

// Note that Windows appends some functions with W to indicate that wide
// characters (Unicode) are in use.  The documentation refers to them
// without this suffix, as the resolution is made via preprocessor.
var (
	procReadConsoleInput           = k32.NewProc("ReadConsoleInputW")
	procGetConsoleCursorInfo       = k32.NewProc("GetConsoleCursorInfo")
	procSetConsoleCursorInfo       = k32.NewProc("SetConsoleCursorInfo")
	procSetConsoleCursorPosition   = k32.NewProc("SetConsoleCursorPosition")
	procSetConsoleMode             = k32.NewProc("SetConsoleMode")
	procGetConsoleMode             = k32.NewProc("GetConsoleMode")
	procGetConsoleScreenBufferInfo = k32.NewProc("GetConsoleScreenBufferInfo")
	procFillConsoleOutputAttribute = k32.NewProc("FillConsoleOutputAttribute")
	procFillConsoleOutputCharacter = k32.NewProc("FillConsoleOutputCharacterW")
	procSetConsoleWindowInfo       = k32.NewProc("SetConsoleWindowInfo")
	procSetConsoleScreenBufferSize = k32.NewProc("SetConsoleScreenBufferSize")
	procSetConsoleTextAttribute    = k32.NewProc("SetConsoleTextAttribute")
)

// We have to bring in the kernel32.dll directly, so we can get access to some
// system calls that the core Go API lacks.

func NewConsoleScreen() (Screen, error) {
	return &cScreen{}, nil
}

func (s *cScreen) Init() error {

	s.evch = make(chan Event, 2)
	s.quit = make(chan struct{})

	if in, e := syscall.Open("CONIN$", syscall.O_RDWR, 0); e != nil {
		return e
	} else {
		s.in = in
	}
	if out, e := syscall.Open("CONOUT$", syscall.O_RDWR, 0); e != nil {
		syscall.Close(s.in)
		return e
	} else {
		s.out = out
	}

	s.curx = -1
	s.cury = -1
	s.getCursorInfo(&s.ocursor)
	s.getConsoleInfo(&s.oscreen)
	s.getOutMode(&s.oomode)
	s.getInMode(&s.oimode)
	s.resize()

	s.setInMode(modeResizeEn)
	s.setOutMode(0)
	s.clearScreen(s.style)
	s.hideCursor()
	go s.scanInput()

	return nil
}

func (s *cScreen) CharacterSet() string {
	// We are always UTF-16LE on Windows
	return "UTF-16LE"
}

func (s *cScreen) EnableMouse() {
	s.setInMode(modeResizeEn | modeMouseEn)
}

func (s *cScreen) DisableMouse() {
	s.setInMode(modeResizeEn)
}

func (s *cScreen) Fini() {
	s.style = StyleDefault
	s.curx = -1
	s.cury = -1

	s.setCursorInfo(&s.ocursor)
	s.setInMode(s.oimode)
	s.setOutMode(s.oomode)
	s.setBufferSize(int(s.oscreen.size.x), int(s.oscreen.size.y))
	s.clearScreen(StyleDefault)
	s.setCursorPos(0, 0)
	procSetConsoleTextAttribute.Call(
		uintptr(s.out),
		uintptr(mapStyle(StyleDefault)))

	close(s.quit)
	syscall.Close(s.in)
	syscall.Close(s.out)
}

func (s *cScreen) PostEvent(ev Event) {
	select {
	case <-s.quit:
	case s.evch <- ev:
	}
}

func (s *cScreen) PollEvent() Event {
	select {
	case <-s.quit:
		return nil
	case ev := <-s.evch:
		return ev
	}
}

type cursorInfo struct {
	size    uint32
	visible uint32
}

type coord struct {
	x int16
	y int16
}

func (c coord) uintptr() uintptr {
	// little endian, put x first
	return uintptr(c.x) | (uintptr(c.y) << 16)
}

type rect struct {
	left   int16
	top    int16
	right  int16
	bottom int16
}

func (s *cScreen) showCursor() {
	s.setCursorInfo(&cursorInfo{size: 100, visible: 1})
}

func (s *cScreen) hideCursor() {
	s.setCursorInfo(&cursorInfo{size: 1, visible: 0})
}

func (s *cScreen) ShowCursor(x, y int) {
	s.Lock()
	s.curx = x
	s.cury = y
	s.Unlock()
}

func (s *cScreen) doCursor() {
	x, y := s.curx, s.cury

	if x < 0 || y < 0 || x >= s.w || y >= s.h {
		s.setCursorPos(0, 0)
		s.hideCursor()
	} else {
		s.setCursorPos(x, y)
		s.showCursor()
	}
}

func (c *cScreen) HideCursor() {
	c.ShowCursor(-1, -1)
}

type charInfo struct {
	ch   uint16
	attr uint16
}

type inputRecord struct {
	typ  uint16
	_    uint16
	data [16]byte
}

const (
	keyEvent    uint16 = 1
	mouseEvent  uint16 = 2
	resizeEvent uint16 = 4
	menuEvent   uint16 = 8  // don't use
	focusEvent  uint16 = 16 // don't use
)

type mouseRecord struct {
	x     int16
	y     int16
	btns  uint32
	mod   uint32
	flags uint32
}

const (
	mouseDoubleClick uint32 = 0x2
	mouseHWheeled    uint32 = 0x8
	mouseVWheeled    uint32 = 0x4
	mouseMoved       uint32 = 0x1
)

type resizeRecord struct {
	x int16
	y int16
}

type keyRecord struct {
	isdown int32
	repeat uint16
	kcode  uint16
	scode  uint16
	ch     uint16
	mod    uint32
}

const (
	// Constants per Microsoft.  We don't put the modifiers
	// here.
	vkCancel = 0x03
	vkBack   = 0x08 // Backspace
	vkTab    = 0x09
	vkClear  = 0x0c
	vkReturn = 0x0d
	vkPause  = 0x13
	vkEscape = 0x1b
	vkSpace  = 0x20
	vkPrior  = 0x21 // PgUp
	vkNext   = 0x22 // PgDn
	vkEnd    = 0x23
	vkHome   = 0x24
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkPrint  = 0x2a
	vkPrtScr = 0x2c
	vkInsert = 0x2d
	vkDelete = 0x2e
	vkHelp   = 0x2f
	vkF1     = 0x70
	vkF2     = 0x71
	vkF3     = 0x72
	vkF4     = 0x73
	vkF5     = 0x74
	vkF6     = 0x75
	vkF7     = 0x76
	vkF8     = 0x77
	vkF9     = 0x78
	vkF10    = 0x79
	vkF11    = 0x7a
	vkF12    = 0x7b
	vkF13    = 0x7c
	vkF14    = 0x7d
	vkF15    = 0x7e
	vkF16    = 0x7f
	vkF17    = 0x80
	vkF18    = 0x81
	vkF19    = 0x82
	vkF20    = 0x83
	vkF21    = 0x84
	vkF22    = 0x85
	vkF23    = 0x86
	vkF24    = 0x87
)

// NB: All Windows platforms are little endian.  We assume this
// never, ever change.  The following code is endian safe. and does
// not use unsafe pointers.
func getu32(v []byte) uint32 {
	return uint32(v[0]) + (uint32(v[1]) << 8) + (uint32(v[2]) << 16) + (uint32(v[3]) << 24)
}
func geti32(v []byte) int32 {
	return int32(getu32(v))
}
func getu16(v []byte) uint16 {
	return uint16(v[0]) + (uint16(v[1]) << 8)
}
func geti16(v []byte) int16 {
	return int16(getu16(v))
}

// Convert windows dwControlKeyState to modifier mask
func mod2mask(cks uint32) ModMask {
	mm := ModNone
	// Left or right control
	if (cks & (0x0008 | 0x0004)) != 0 {
		mm |= ModCtrl
	}
	// Left or right alt
	if (cks & (0x0002 | 0x0001)) != 0 {
		mm |= ModAlt
	}
	// Any shift
	if (cks & 0x0010) != 0 {
		mm |= ModShift
	}
	return mm
}

func (s *cScreen) getConsoleInput() error {
	rec := &inputRecord{}
	var nrec int32
	rv, _, er := procReadConsoleInput.Call(
		uintptr(s.in),
		uintptr(unsafe.Pointer(rec)),
		uintptr(1),
		uintptr(unsafe.Pointer(&nrec)))
	if rv == 0 {
		return er
	}
	if nrec != 1 {
		return nil
	}
	switch rec.typ {
	case keyEvent:
		krec := &keyRecord{}
		krec.isdown = geti32(rec.data[0:])
		krec.repeat = getu16(rec.data[4:])
		krec.kcode = getu16(rec.data[6:])
		krec.scode = getu16(rec.data[8:])
		krec.ch = getu16(rec.data[10:])
		krec.mod = getu32(rec.data[12:])

		if krec.isdown == 0 || krec.repeat < 1 {
			// its a key release event, ignore it
			return nil
		}
		if krec.ch != 0 {
			// synthesized key code
			for krec.repeat > 0 {
				s.PostEvent(NewEventKey(KeyRune, rune(krec.ch), mod2mask(krec.mod)))
				krec.repeat--
			}
			return nil
		}
		key := KeyNUL // impossible on Windows
		switch krec.kcode {
		case vkCancel:
			key = KeyCancel
		case vkBack:
			key = KeyBackspace
		case vkTab:
			key = KeyTab
		case vkClear:
			key = KeyClear
		case vkPause:
			key = KeyPause
		case vkPrint, vkPrtScr:
			key = KeyPrint
		case vkPrior:
			key = KeyPgUp
		case vkNext:
			key = KeyPgDn
		case vkReturn:
			key = KeyEnter
		case vkEnd:
			key = KeyEnd
		case vkHome:
			key = KeyHome
		case vkLeft:
			key = KeyLeft
		case vkUp:
			key = KeyUp
		case vkRight:
			key = KeyRight
		case vkDown:
			key = KeyDown
		case vkInsert:
			key = KeyInsert
		case vkDelete:
			key = KeyDelete
		case vkHelp:
			key = KeyHelp
		case vkF1:
			key = KeyF1
		case vkF2:
			key = KeyF2
		case vkF3:
			key = KeyF3
		case vkF4:
			key = KeyF4
		case vkF5:
			key = KeyF5
		case vkF6:
			key = KeyF6
		case vkF7:
			key = KeyF7
		case vkF8:
			key = KeyF8
		case vkF9:
			key = KeyF9
		case vkF10:
			key = KeyF10
		case vkF11:
			key = KeyF11
		case vkF12:
			key = KeyF12
		case vkF13:
			key = KeyF13
		case vkF14:
			key = KeyF14
		case vkF15:
			key = KeyF15
		case vkF16:
			key = KeyF16
		case vkF17:
			key = KeyF17
		case vkF18:
			key = KeyF18
		case vkF19:
			key = KeyF19
		case vkF20:
			key = KeyF20
		case vkF21:
			key = KeyF21
		case vkF22:
			key = KeyF22
		case vkF23:
			key = KeyF23
		case vkF24:
			key = KeyF24
		default:
			return nil
		}
		for krec.repeat > 0 {
			s.PostEvent(NewEventKey(key, rune(krec.ch),
				mod2mask(krec.mod)))
			krec.repeat--
		}

	case mouseEvent:
		var mrec mouseRecord
		mrec.x = geti16(rec.data[0:])
		mrec.y = geti16(rec.data[2:])
		mrec.btns = getu32(rec.data[4:])
		mrec.mod = getu32(rec.data[8:])
		mrec.flags = getu32(rec.data[12:]) // not using yet
		btns := ButtonNone

		s.mbtns = mrec.btns
		if mrec.btns&0x1 != 0 {
			btns |= Button1
		}
		if mrec.btns&0x2 != 0 {
			btns |= Button2
		}
		if mrec.btns&0x4 != 0 {
			btns |= Button3
		}
		if mrec.btns&0x8 != 0 {
			btns |= Button4
		}
		if mrec.btns&0x10 != 0 {
			btns |= Button5
		}

		if mrec.flags&mouseVWheeled != 0 {
			if mrec.btns&0x80000000 == 0 {
				btns |= WheelUp
			} else {
				btns |= WheelDown
			}
		}
		if mrec.flags&mouseHWheeled != 0 {
			if mrec.btns&0x80000000 == 0 {
				btns |= WheelRight
			} else {
				btns |= WheelLeft
			}
		}
		// we ignore double click, events are delivered normally
		s.PostEvent(NewEventMouse(int(mrec.x), int(mrec.y), btns,
			mod2mask(mrec.mod)))

	case resizeEvent:
		var rrec resizeRecord
		rrec.x = geti16(rec.data[0:])
		rrec.y = geti16(rec.data[2:])
		s.PostEvent(NewEventResize(int(rrec.x), int(rrec.y)))

	default:
	}
	return nil
}

func (s *cScreen) scanInput() {
	for {
		if e := s.getConsoleInput(); e != nil {
			return
		}
	}
}

// Windows console can display 8 characters, in either low or high intensity
func (s *cScreen) Colors() int {
	return 16
}

// Windows uses RGB signals
func mapColor2RGB(c Color) uint16 {
	switch c {
	case ColorBlack:
		return 0
		// primaries
	case ColorRed:
		return 0x4
	case ColorGreen:
		return 0x2
	case ColorBlue:
		return 0x1
	case ColorYellow:
		return 0x6
	case ColorMagenta:
		return 0x5
	case ColorCyan:
		return 0x3
	case ColorWhite:
		return 0x7
	// bright variants
	case ColorGrey:
		return 0x8
	case ColorBrightRed:
		return 0xc
	case ColorBrightGreen:
		return 0xa
	case ColorBrightBlue:
		return 0x9
	case ColorBrightYellow:
		return 0xe
	case ColorBrightMagenta:
		return 0xd
	case ColorBrightCyan:
		return 0xb
	case ColorBrightWhite:
		return 0xf
	}
	return 0
}

// Map a tcell style to Windows attributes
func mapStyle(style Style) uint16 {
	f, b, a := style.Decompose()
	if f == ColorDefault {
		f = ColorWhite
	}
	if b == ColorDefault {
		b = ColorBlack
	}
	var attr uint16
	// We simulate reverse by doing the color swap ourselves.
	// Apparently windows cannot really do this except in DBCS
	// views.
	if a&AttrReverse != 0 {
		attr = mapColor2RGB(b)
		attr |= (mapColor2RGB(f) << 4)
	} else {
		attr = mapColor2RGB(f)
		attr |= (mapColor2RGB(b) << 4)
	}
	if a&AttrBold != 0 {
		attr |= 0x8
	}
	if a&AttrDim != 0 {
		attr &^= 0x8
	}
	if a&AttrUnderline != 0 {
		// Best effort -- doesn't seem to work though.
		attr |= 0x8000
	}
	// Blink is unsupported
	return attr
}

func (s *cScreen) SetCell(x, y int, style Style, ch ...rune) {

	s.Lock()
	if x < 0 || y < 0 || x >= int(s.w) || y >= int(s.h) {
		s.Unlock()
		return
	}

	cell := &s.cells[(y*int(s.w))+x]
	cell.SetCell(ch, style)
	s.Unlock()
}

func (s *cScreen) PutCell(x, y int, cell *Cell) {
	s.Lock()
	if x < 0 || y < 0 || x >= int(s.w) || y >= int(s.h) {
		s.Unlock()
		return
	}
	cptr := &s.cells[(y*int(s.w))+x]
	cptr.PutChars(cell.Ch)
	cptr.PutStyle(cell.Style)
	s.Unlock()
}

func (s *cScreen) GetCell(x, y int) *Cell {
	s.Lock()
	if x < 0 || y < 0 || x >= int(s.w) || y >= int(s.h) {
		s.Unlock()
		return nil
	}
	cell := s.cells[(y*int(s.w))+x]
	s.Unlock()
	return &cell
}

func (s *cScreen) writeString(x, y int, style Style, ch []uint16) {
	// we assume the caller has hidden the cursor
	if len(ch) == 0 {
		return
	}
	nw := uint32(len(ch))
	procSetConsoleTextAttribute.Call(
		uintptr(s.out),
		uintptr(mapStyle(style)))
	s.setCursorPos(x, y)
	syscall.WriteConsole(s.out, &ch[0], nw, &nw, nil)
}

func (s *cScreen) draw() {
	// allocate a scratch line bit enough for no combining chars.
	// if you have combining characters, you may pay for extra allocs.
	if s.clear {
		s.clearScreen(s.style)
		s.clear = false
	}
	buf := make([]uint16, 0, s.w)
	wcs := buf[:]
	style := Style(-1) // invalid attribute

	x, y := -1, -1

	for row := 0; row < int(s.h); row++ {
		width := 1
		for col := 0; col < int(s.w); col += width {

			cell := &s.cells[(row*s.w)+col]
			width = int(cell.Width)
			if width < 1 {
				width = 1
			}

			if !cell.Dirty || style != cell.Style {
				s.writeString(x, y, style, wcs)
				wcs = buf[0:0]
				style = Style(-1)
				if !cell.Dirty {
					continue
				}
			}
			if len(wcs) == 0 {
				style = cell.Style
				x = col
				y = row
			}
			if len(cell.Ch) < 1 {
				wcs = append(wcs, uint16(' '))
			} else {
				wcs = append(wcs, utf16.Encode(cell.Ch)...)
			}
			cell.Dirty = false
		}
		s.writeString(x, y, style, wcs)
		wcs = buf[0:0]
		style = Style(-1)
	}
}

func (s *cScreen) Show() {
	s.Lock()
	s.hideCursor()
	s.resize()
	s.draw()
	s.doCursor()
	s.Unlock()
}

func (s *cScreen) Sync() {
	s.Lock()
	InvalidateCells(s.cells)
	s.hideCursor()
	s.resize()
	s.draw()
	s.doCursor()
	s.Unlock()
}

type consoleInfo struct {
	size  coord
	pos   coord
	attrs uint16
	win   rect
	maxsz coord
}

func (s *cScreen) getConsoleInfo(info *consoleInfo) {
	procGetConsoleScreenBufferInfo.Call(
		uintptr(s.out),
		uintptr(unsafe.Pointer(info)))
}

func (s *cScreen) getCursorInfo(info *cursorInfo) {
	procGetConsoleCursorInfo.Call(
		uintptr(s.out),
		uintptr(unsafe.Pointer(info)))
}

func (s *cScreen) setCursorInfo(info *cursorInfo) {
	procSetConsoleCursorInfo.Call(
		uintptr(s.out),
		uintptr(unsafe.Pointer(info)))
}

func (s *cScreen) setCursorPos(x, y int) {
	procSetConsoleCursorPosition.Call(
		uintptr(s.out),
		coord{int16(x), int16(y)}.uintptr())
}

func (s *cScreen) setBufferSize(x, y int) {
	procSetConsoleScreenBufferSize.Call(
		uintptr(s.out),
		coord{int16(x), int16(y)}.uintptr())
}

func (s *cScreen) Size() (int, int) {

	s.Lock()
	w, h := s.w, s.h
	s.Unlock()

	return w, h
}

func (s *cScreen) resize() {

	info := consoleInfo{}
	s.getConsoleInfo(&info)

	w := int((info.win.right - info.win.left) + 1)
	h := int((info.win.bottom - info.win.top) + 1)

	if s.w == w && s.h == h {
		return
	}

	s.cells = ResizeCells(s.cells, s.w, s.h, w, h)
	s.w = w
	s.h = h

	r := rect{0, 0, int16(w - 1), int16(h - 1)}
	procSetConsoleWindowInfo.Call(
		uintptr(s.out),
		uintptr(1),
		uintptr(unsafe.Pointer(&r)))

	s.setBufferSize(w, h)

	s.PostEvent(NewEventResize(w, h))
}

func (s *cScreen) Clear() {
	s.Lock()
	ClearCells(s.cells, s.style)
	s.clear = true
	s.Unlock()
}

func (s *cScreen) clearScreen(style Style) {
	pos := coord{0, 0}
	attr := mapStyle(style)
	x, y := s.w, s.h
	scratch := uint32(0)
	count := uint32(x * y)

	procFillConsoleOutputAttribute.Call(
		uintptr(s.out),
		uintptr(attr),
		uintptr(count),
		pos.uintptr(),
		uintptr(unsafe.Pointer(&scratch)))
	procFillConsoleOutputCharacter.Call(
		uintptr(s.out),
		uintptr(' '),
		uintptr(count),
		pos.uintptr(),
		uintptr(unsafe.Pointer(&scratch)))
}

const (
	modeMouseEn  uint32 = 0x0010
	modeResizeEn uint32 = 0x0008
	modeWrapEOL  uint32 = 0x0002
	modeCooked   uint32 = 0x0001
)

func (s *cScreen) setInMode(mode uint32) error {
	rv, _, err := procSetConsoleMode.Call(
		uintptr(s.in),
		uintptr(mode))
	if rv == 0 {
		return err
	}
	return nil
}

func (s *cScreen) setOutMode(mode uint32) error {
	rv, _, err := procSetConsoleMode.Call(
		uintptr(s.out),
		uintptr(mode))
	if rv == 0 {
		return err
	}
	return nil
}

func (s *cScreen) getInMode(v *uint32) {
	procGetConsoleMode.Call(
		uintptr(s.in),
		uintptr(unsafe.Pointer(v)))
}

func (s *cScreen) getOutMode(v *uint32) {
	procGetConsoleMode.Call(
		uintptr(s.out),
		uintptr(unsafe.Pointer(v)))
}

func (s *cScreen) SetStyle(style Style) {
	s.Lock()
	s.style = style
	s.Unlock()
}
