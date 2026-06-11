//go:build windows
// +build windows

package tty

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/mattn/go-isatty"
)

const (
	rightAltPressed  = 1
	leftAltPressed   = 2
	rightCtrlPressed = 4
	leftCtrlPressed  = 8
	shiftPressed     = 0x0010
	ctrlPressed      = rightCtrlPressed | leftCtrlPressed
	altPressed       = rightAltPressed | leftAltPressed
)

const (
	enableProcessedInput = 0x1
	enableLineInput      = 0x2
	enableEchoInput      = 0x4
	enableWindowInput    = 0x8
	enableMouseInput     = 0x10
	enableInsertMode     = 0x20
	enableQuickEditMode  = 0x40
	enableExtendedFlag   = 0x80

	enableProcessedOutput = 1
	enableWrapAtEolOutput = 2

	keyEvent              = 0x1
	mouseEvent            = 0x2
	windowBufferSizeEvent = 0x4

	enableVirtualTerminalInput = 0x200

	waitObject0 = 0
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

var (
	procAllocConsole                = kernel32.NewProc("AllocConsole")
	procSetStdHandle                = kernel32.NewProc("SetStdHandle")
	procGetStdHandle                = kernel32.NewProc("GetStdHandle")
	procSetConsoleScreenBufferSize  = kernel32.NewProc("SetConsoleScreenBufferSize")
	procCreateConsoleScreenBuffer   = kernel32.NewProc("CreateConsoleScreenBuffer")
	procGetConsoleScreenBufferInfo  = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procWriteConsoleOutputCharacter = kernel32.NewProc("WriteConsoleOutputCharacterW")
	procWriteConsoleOutputAttribute = kernel32.NewProc("WriteConsoleOutputAttribute")
	procGetConsoleCursorInfo        = kernel32.NewProc("GetConsoleCursorInfo")
	procSetConsoleCursorInfo        = kernel32.NewProc("SetConsoleCursorInfo")
	procSetConsoleCursorPosition    = kernel32.NewProc("SetConsoleCursorPosition")
	procReadConsoleInput            = kernel32.NewProc("ReadConsoleInputW")
	procGetConsoleMode              = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode              = kernel32.NewProc("SetConsoleMode")
	procFillConsoleOutputCharacter  = kernel32.NewProc("FillConsoleOutputCharacterW")
	procFillConsoleOutputAttribute  = kernel32.NewProc("FillConsoleOutputAttribute")
	procScrollConsoleScreenBuffer   = kernel32.NewProc("ScrollConsoleScreenBufferW")
	procWaitForSingleObject         = kernel32.NewProc("WaitForSingleObject")
)

type wchar uint16
type short int16
type dword uint32
type word uint16

type coord struct {
	x short
	y short
}

type smallRect struct {
	left   short
	top    short
	right  short
	bottom short
}

type consoleScreenBufferInfo struct {
	size              coord
	cursorPosition    coord
	attributes        word
	window            smallRect
	maximumWindowSize coord
}

type consoleCursorInfo struct {
	size    dword
	visible int32
}

type inputRecord struct {
	eventType word
	_         [2]byte
	event     [16]byte
}

type keyEventRecord struct {
	keyDown         int32
	repeatCount     word
	virtualKeyCode  word
	virtualScanCode word
	unicodeChar     wchar
	controlKeyState dword
}

type windowBufferSizeRecord struct {
	size coord
}

type mouseEventRecord struct {
	mousePos        coord
	buttonState     dword
	controlKeyState dword
	eventFlags      dword
}

type charInfo struct {
	unicodeChar wchar
	attributes  word
}

type TTY struct {
	in                *os.File
	out               *os.File
	st                uint32
	rs                []rune
	ws                chan WINSIZE
	sigwinchCtx       context.Context
	sigwinchCtxCancel context.CancelFunc
	readNextKeyUp     bool
}

func readConsoleInput(fd uintptr, record *inputRecord) (err error) {
	var w uint32
	r1, _, err := procReadConsoleInput.Call(fd, uintptr(unsafe.Pointer(record)), 1, uintptr(unsafe.Pointer(&w)))
	if r1 == 0 {
		return err
	}
	return nil
}

func open(path string) (*TTY, error) {
	tty := new(TTY)
	if false && isatty.IsTerminal(os.Stdin.Fd()) {
		tty.in = os.Stdin
	} else {
		in, err := syscall.Open("CONIN$", syscall.O_RDWR, 0)
		if err != nil {
			return nil, err
		}

		tty.in = os.NewFile(uintptr(in), "/dev/tty")
	}

	if isatty.IsTerminal(os.Stdout.Fd()) {
		tty.out = os.Stdout
	} else {
		procAllocConsole.Call()
		out, err := syscall.Open("CONOUT$", syscall.O_RDWR, 0)
		if err != nil {
			return nil, err
		}

		tty.out = os.NewFile(uintptr(out), "/dev/tty")
	}

	h := tty.in.Fd()
	var st uint32
	r1, _, err := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&st)))
	if r1 == 0 {
		return nil, err
	}
	tty.st = st

	st &^= enableEchoInput
	st &^= enableInsertMode
	st &^= enableLineInput
	st &^= enableMouseInput
	st &^= enableWindowInput
	st &^= enableExtendedFlag
	st &^= enableQuickEditMode

	// ignore error
	procSetConsoleMode.Call(h, uintptr(st))

	tty.ws = make(chan WINSIZE)
	tty.sigwinchCtx, tty.sigwinchCtxCancel = context.WithCancel(context.Background())

	return tty, nil
}

func (tty *TTY) buffered() bool {
	return len(tty.rs) > 0
}

func (tty *TTY) readRune() (rune, int, error) {
	if len(tty.rs) > 0 {
		r := tty.rs[0]
		tty.rs = tty.rs[1:]
		return r, 1, nil
	}
	var ir inputRecord
	err := readConsoleInput(tty.in.Fd(), &ir)
	if err != nil {
		return 0, 0, err
	}

	switch ir.eventType {
	case windowBufferSizeEvent:
		wr := (*windowBufferSizeRecord)(unsafe.Pointer(&ir.event))
		ws := WINSIZE{
			W: int(wr.size.x),
			H: int(wr.size.y),
		}

		if err := tty.sigwinchCtx.Err(); err != nil {
			// closing
			// the following select might panic without this guard close
			return 0, 0, err
		}

		select {
		case tty.ws <- ws:
		case <-tty.sigwinchCtx.Done():
			return 0, 0, tty.sigwinchCtx.Err()
		default:
			return 0, 0, nil
		}
	case keyEvent:
		kr := (*keyEventRecord)(unsafe.Pointer(&ir.event))
		if kr.keyDown == 0 {
			if kr.unicodeChar != 0 && tty.readNextKeyUp {
				tty.readNextKeyUp = false
				if 0x2000 <= kr.unicodeChar && kr.unicodeChar < 0x3000 {
					return rune(kr.unicodeChar), 1, nil
				}
			}
		} else {
			if kr.controlKeyState&altPressed != 0 && kr.unicodeChar > 0 {
				tty.rs = []rune{rune(kr.unicodeChar)}
				return rune(0x1b), 1, nil
			}
			if kr.unicodeChar > 0 {
				if kr.controlKeyState&shiftPressed != 0 {
					switch kr.unicodeChar {
					case 0x09:
						tty.rs = []rune{0x5b, 0x5a}
						return rune(0x1b), 1, nil
					}
				}
				return rune(kr.unicodeChar), 1, nil
			}
			vk := kr.virtualKeyCode
			if kr.controlKeyState&ctrlPressed != 0 {
				switch vk {
				case 0x21: // ctrl-page-up
					tty.rs = []rune{0x5b, 0x35, 0x3B, 0x35, 0x7e}
					return rune(0x1b), 1, nil
				case 0x22: // ctrl-page-down
					tty.rs = []rune{0x5b, 0x36, 0x3B, 0x35, 0x7e}
					return rune(0x1b), 1, nil
				case 0x23: // ctrl-end
					tty.rs = []rune{0x5b, 0x31, 0x3B, 0x35, 0x46}
					return rune(0x1b), 1, nil
				case 0x24: // ctrl-home
					tty.rs = []rune{0x5b, 0x31, 0x3B, 0x35, 0x48}
					return rune(0x1b), 1, nil
				case 0x25: // ctrl-left
					tty.rs = []rune{0x5b, 0x31, 0x3B, 0x35, 0x44}
					return rune(0x1b), 1, nil
				case 0x26: // ctrl-up
					tty.rs = []rune{0x5b, 0x31, 0x3B, 0x35, 0x41}
					return rune(0x1b), 1, nil
				case 0x27: // ctrl-right
					tty.rs = []rune{0x5b, 0x31, 0x3B, 0x35, 0x43}
					return rune(0x1b), 1, nil
				case 0x28: // ctrl-down
					tty.rs = []rune{0x5b, 0x31, 0x3B, 0x35, 0x42}
					return rune(0x1b), 1, nil
				case 0x2e: // ctrl-delete
					tty.rs = []rune{0x5b, 0x33, 0x3B, 0x35, 0x7e}
					return rune(0x1b), 1, nil
				}
			}
			switch vk {
			case 0x12: // menu
				if kr.controlKeyState&leftAltPressed != 0 {
					tty.readNextKeyUp = true
				}
				return 0, 0, nil
			case 0x21: // page-up
				tty.rs = []rune{0x5b, 0x35, 0x7e}
				return rune(0x1b), 1, nil
			case 0x22: // page-down
				tty.rs = []rune{0x5b, 0x36, 0x7e}
				return rune(0x1b), 1, nil
			case 0x23: // end
				tty.rs = []rune{0x5b, 0x46}
				return rune(0x1b), 1, nil
			case 0x24: // home
				tty.rs = []rune{0x5b, 0x48}
				return rune(0x1b), 1, nil
			case 0x25: // left
				tty.rs = []rune{0x5b, 0x44}
				return rune(0x1b), 1, nil
			case 0x26: // up
				tty.rs = []rune{0x5b, 0x41}
				return rune(0x1b), 1, nil
			case 0x27: // right
				tty.rs = []rune{0x5b, 0x43}
				return rune(0x1b), 1, nil
			case 0x28: // down
				tty.rs = []rune{0x5b, 0x42}
				return rune(0x1b), 1, nil
			case 0x2D: // Insert
				tty.rs = []rune{0x5b, 0x32, 0x7e}
				return rune(0x1b), 1, nil
			case 0x2e: // delete
				tty.rs = []rune{0x5b, 0x33, 0x7e}
				return rune(0x1b), 1, nil
			case 0x70, 0x71, 0x72, 0x73: // F1,F2,F3,F4
				tty.rs = []rune{0x5b, 0x4f, rune(vk) - 0x20}
				return rune(0x1b), 1, nil
			case 0x074, 0x75, 0x76, 0x77: // F5,F6,F7,F8
				tty.rs = []rune{0x5b, 0x31, rune(vk) - 0x3f, 0x7e}
				return rune(0x1b), 1, nil
			case 0x78, 0x79: // F9,F10
				tty.rs = []rune{0x5b, 0x32, rune(vk) - 0x48, 0x7e}
				return rune(0x1b), 1, nil
			case 0x7a, 0x7b: // F11,F12
				tty.rs = []rune{0x5b, 0x32, rune(vk) - 0x47, 0x7e}
				return rune(0x1b), 1, nil
			}
			return 0, 0, nil
		}
	}
	return 0, 0, nil
}

func (tty *TTY) close() error {
	procSetConsoleMode.Call(tty.in.Fd(), uintptr(tty.st))
	tty.sigwinchCtxCancel()
	close(tty.ws)
	return nil
}

func (tty *TTY) size() (int, int, error) {
	var csbi consoleScreenBufferInfo
	r1, _, err := procGetConsoleScreenBufferInfo.Call(tty.out.Fd(), uintptr(unsafe.Pointer(&csbi)))
	if r1 == 0 {
		return 0, 0, err
	}
	return int(csbi.window.right - csbi.window.left + 1), int(csbi.window.bottom - csbi.window.top + 1), nil
}

func (tty *TTY) sizePixel() (int, int, int, int, error) {
	x, y, err := tty.size()
	if err != nil {
		return -1, -1, -1, -1, err
	}
	wpx, hpx, err := tty.queryTextAreaPixel()
	if err != nil {
		return x, y, -1, -1, err
	}
	return x, y, wpx, hpx, nil
}

// queryTextAreaPixel asks the hosting terminal for the pixel size of the
// text area with CSI 14 t and parses the "ESC [ 4 ; height ; width t"
// reply. The Windows console API cannot report pixel sizes under ConPTY,
// where the rendering terminal owns the font; only the terminal itself
// knows them. Raw key events are read with a short deadline so terminals
// that do not answer the query cannot hang the caller.
func (tty *TTY) queryTextAreaPixel() (int, int, error) {
	in := tty.in.Fd()

	var mode uint32
	if r1, _, err := procGetConsoleMode.Call(in, uintptr(unsafe.Pointer(&mode))); r1 == 0 {
		return -1, -1, err
	}
	raw := (mode &^ (enableLineInput | enableEchoInput | enableProcessedInput)) | enableVirtualTerminalInput
	if r1, _, err := procSetConsoleMode.Call(in, uintptr(raw)); r1 == 0 {
		return -1, -1, err
	}
	defer procSetConsoleMode.Call(in, uintptr(mode))

	if _, err := tty.out.WriteString("\x1b[14t"); err != nil {
		return -1, -1, err
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	var buf []byte
	for len(buf) < 64 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		r1, _, _ := procWaitForSingleObject.Call(in, uintptr(remaining.Milliseconds()))
		if r1 != waitObject0 {
			break
		}
		var ir inputRecord
		var n uint32
		r1, _, _ = procReadConsoleInput.Call(in, uintptr(unsafe.Pointer(&ir)), 1, uintptr(unsafe.Pointer(&n)))
		if r1 == 0 || n != 1 {
			break
		}
		if ir.eventType != keyEvent {
			continue
		}
		kr := (*keyEventRecord)(unsafe.Pointer(&ir.event))
		if kr.keyDown == 0 || kr.unicodeChar == 0 || kr.unicodeChar > 0x7f {
			continue
		}
		buf = append(buf, byte(kr.unicodeChar))
		if buf[len(buf)-1] == 't' {
			break
		}
	}

	// Expected: ESC [ 4 ; height ; width t
	s := string(buf)
	i := strings.Index(s, "\x1b[4;")
	if i < 0 {
		return -1, -1, errors.New("terminal did not reply to CSI 14 t")
	}
	s = s[i+4:]
	end := strings.IndexByte(s, 't')
	if end < 0 {
		return -1, -1, errors.New("invalid reply to CSI 14 t")
	}
	parts := strings.SplitN(s[:end], ";", 2)
	if len(parts) != 2 {
		return -1, -1, errors.New("invalid reply to CSI 14 t")
	}
	hpx, err := strconv.Atoi(parts[0])
	if err != nil {
		return -1, -1, err
	}
	wpx, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1, -1, err
	}
	if hpx <= 0 || wpx <= 0 {
		return -1, -1, errors.New("invalid reply to CSI 14 t")
	}
	return wpx, hpx, nil
}

func (tty *TTY) input() *os.File {
	return tty.in
}

func (tty *TTY) output() *os.File {
	return tty.out
}

func (tty *TTY) raw() (func() error, error) {
	var st uint32
	r1, _, err := procGetConsoleMode.Call(tty.in.Fd(), uintptr(unsafe.Pointer(&st)))
	if r1 == 0 {
		return nil, err
	}
	mode := st &^ (enableEchoInput | enableProcessedInput | enableLineInput | enableProcessedOutput)
	r1, _, err = procSetConsoleMode.Call(tty.in.Fd(), uintptr(mode))
	if r1 == 0 {
		return nil, err
	}
	return func() error {
		r1, _, err := procSetConsoleMode.Call(tty.in.Fd(), uintptr(st))
		if r1 == 0 {
			return err
		}
		return nil
	}, nil
}

func (tty *TTY) sigwinch() <-chan WINSIZE {
	return tty.ws
}
