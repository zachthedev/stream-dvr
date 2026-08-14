package notify

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// tray owns the hidden window and the notification area icon a balloon is
// raised through.
//
// Windows has no way to show a notification without an identity behind it.
// A modern toast needs an application user model ID, which for an
// unpackaged program means writing a Start Menu shortcut carrying that ID
// through two COM interfaces. A notification area balloon needs none of
// that: an icon and a window to own it, both per-process and neither
// touching a machine-wide setting.
type tray struct {
	window windows.HWND
	// icon carries the fields Shell_NotifyIcon reads. It is kept whole
	// because Explorer restarting means adding the same icon again.
	icon notifyIconData

	closeOnce sync.Once
	stopped   chan struct{}
}

// notifyIconData mirrors NOTIFYICONDATAW.
//
// The layout is the operating system's, so the fields are ordered and
// sized to match rather than to read well. cbSize tells Windows which
// version of the structure this is, and it is computed rather than written
// down so that it cannot disagree with what is actually passed.
type notifyIconData struct {
	cbSize           uint32
	hWnd             windows.HWND
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            windows.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         windows.GUID
	hBalloonIcon     windows.Handle
}

// wndClassEx mirrors WNDCLASSEXW.
type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

// message mirrors MSG.
type message struct {
	hWnd     windows.HWND
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       point
	lPrivate uint32
}

// point mirrors POINT.
type point struct {
	x int32
	y int32
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// Notification area operations and the fields each one carries.
const (
	nimAdd    = 0x0
	nimModify = 0x1
	nimDelete = 0x2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10

	// niifInfo draws the balloon with an information glyph. The quiet flag
	// is deliberately not set: an operator who asked for notifications
	// wants to be told, and Focus Assist is where they say otherwise.
	niifInfo = 0x1
)

// Window messages and styles.
const (
	wmDestroy = 0x0002
	wmClose   = 0x0010
	wmQuit    = 0x0012
	// wmTrayCallback is where the icon reports mouse activity. It has to
	// sit in the application range, and nothing else in this process
	// defines one.
	wmTrayCallback = 0x0400 + 1

	swHide = 0

	// idiApplication is the stock program icon, which needs no resource
	// compiled into the binary.
	idiApplication = 32512
)

// trayTimeout is how long Windows is asked to leave a balloon up. It is
// advisory: the shell clamps it to the accessibility timeout the operator
// set, and ignores it entirely on versions that route balloons through the
// toast platform.
const trayTimeout = 10000

// ///////////////////////////////////////////////
// Variables
// ///////////////////////////////////////////////

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassEx       = user32.NewProc("RegisterClassExW")
	procCreateWindowEx        = user32.NewProc("CreateWindowExW")
	procDefWindowProc         = user32.NewProc("DefWindowProcW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procGetMessage            = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessage       = user32.NewProc("DispatchMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procPostMessage           = user32.NewProc("PostMessageW")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procLoadIcon              = user32.NewProc("LoadIconW")
	procShowWindow            = user32.NewProc("ShowWindow")
	procShellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandle       = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// current is the process's one tray.
//
// A package-level pointer because the window procedure is a C callback
// with no room to carry one, and because a process has one notification
// area icon whatever else it does.
var (
	trayOnce sync.Once
	current  *tray
	trayErr  error
)

// taskbarCreated is the broadcast Explorer sends when it restarts.
//
// An icon does not survive that, and a helper meant to run for weeks
// outlives more than one Explorer crash, so the message is registered once
// and the icon added again whenever it arrives.
var taskbarCreated uint32

// ///////////////////////////////////////////////
// Platform
// ///////////////////////////////////////////////

// sessionAvailable reports whether this process can raise a notification.
//
// It builds the tray, because the only honest answer to "can this raise
// one" is whether the window and the icon were accepted. The agent asks
// once at startup, so a desktop that refuses is reported there rather than
// at the first broadcast.
func sessionAvailable() error {
	_, err := ensureTray()
	return err
}

// raise shows a balloon over the notification area icon.
func raise(_ context.Context, title, body string) error {
	icon, err := ensureTray()
	if err != nil {
		return err
	}
	return icon.balloon(title, body)
}

// closeDesktop removes the icon.
//
// An icon whose process exits without this stays drawn until something
// hovers over it, so an agent that ends at logout would leave one behind
// every time.
func closeDesktop() error {
	if current == nil {
		return nil
	}
	return current.close()
}

// hideConsole hides the console this process was given, when it was given
// one of its own.
//
// A program started from a Run key gets one whether it wants one or not,
// and a notification helper showing a blank black window is worse than the
// notifications are worth. A frame of it can still appear before this
// runs; a second binary linked as a GUI application is what removes that,
// at the cost of doubling the release artifacts.
//
// A console an operator ran this from belongs to their shell, and hiding it
// takes the shell and every line printed into it off the screen with no way
// to bring it back. ownsConsole is what separates the two.
func hideConsole() {
	window, _, _ := procGetConsoleWindow.Call()
	if window == 0 || !ownsConsole() {
		return
	}
	procShowWindow.Call(window, swHide) //nolint:errcheck // hiding is cosmetic and its failure changes nothing
}

// ownsConsole reports whether this process is the only one attached to its
// console.
//
// A console handed to a process started from a Run key has that process and
// nothing else. One an operator ran a command from has at least their shell
// as well. GetConsoleProcessList returns the count whether or not the buffer
// held them all, so two slots separate the two cases without asking how many
// the second one has.
//
// A call that fails answers false. A console this cannot ask about is not
// one to hide.
func ownsConsole() bool {
	var attached [2]uint32
	count, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&attached[0])), uintptr(len(attached)))
	return count == 1
}

// ///////////////////////////////////////////////
// Lifecycle
// ///////////////////////////////////////////////

// ensureTray builds the process's tray on first use.
func ensureTray() (*tray, error) {
	trayOnce.Do(func() {
		current, trayErr = newTray()
	})
	return current, trayErr
}

// newTray registers the window class, creates the window, and adds the
// icon.
//
// The window is created on a thread of its own that then pumps messages.
// A window belongs to the thread that created it and only that thread can
// serve its queue, which no goroutine can promise across a scheduling
// point without locking itself to a thread for good.
func newTray() (*tray, error) {
	icon := &tray{stopped: make(chan struct{})}
	ready := make(chan error, 1)

	go icon.pump(ready)

	if err := <-ready; err != nil {
		return nil, err
	}
	return icon, nil
}

// pump creates the window and serves its messages until it is destroyed.
func (t *tray) pump(ready chan<- error) {
	// Held for the goroutine's life. Releasing it would let the runtime
	// move this goroutine to another thread, where GetMessage serves a
	// different queue than the one the window posts to and the window
	// stops responding.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(t.stopped)

	if err := t.create(); err != nil {
		ready <- err
		return
	}
	ready <- nil

	var msg message
	for {
		got, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		// Zero is WM_QUIT and -1 is a failed call, and neither leaves a
		// queue worth serving.
		if int32(got) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg))) //nolint:errcheck // no failure is defined for it
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))  //nolint:errcheck // the result is the window's, not an error
	}
}

// create registers the class, makes the window, and adds the icon.
func (t *tray) create() error {
	instance, _, err := procGetModuleHandle.Call(0)
	if instance == 0 {
		return fmt.Errorf("locating this module: %w", err)
	}

	className, convErr := windows.UTF16PtrFromString("stream-dvr-notify")
	if convErr != nil {
		return fmt.Errorf("encoding the window class name: %w", convErr)
	}

	class := wndClassEx{
		style:         0,
		lpfnWndProc:   syscall.NewCallback(windowProc),
		hInstance:     windows.Handle(instance),
		lpszClassName: className,
	}
	class.cbSize = uint32(unsafe.Sizeof(class))

	if atom, _, regErr := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("registering the window class: %w", regErr)
	}

	// Never shown. It exists to own the icon and to receive the broadcast
	// Explorer sends when it restarts, which a message-only window would
	// not be given.
	window, _, winErr := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)), 0, 0,
		0, 0, 0, 0, 0, 0, instance, 0)
	if window == 0 {
		return fmt.Errorf("creating the notification window: %w", winErr)
	}
	t.window = windows.HWND(window)

	if taskbarCreated == 0 {
		name, err := windows.UTF16PtrFromString("TaskbarCreated")
		if err != nil {
			return fmt.Errorf("encoding the taskbar message name: %w", err)
		}
		registered, _, _ := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(name)))
		taskbarCreated = uint32(registered)
	}

	stock, _, _ := procLoadIcon.Call(0, idiApplication)

	t.icon = notifyIconData{
		hWnd:             t.window,
		uID:              1,
		uFlags:           nifIcon | nifTip | nifMessage,
		uCallbackMessage: wmTrayCallback,
		hIcon:            windows.Handle(stock),
	}
	t.icon.cbSize = uint32(unsafe.Sizeof(t.icon))
	copyUTF16(t.icon.szTip[:], "stream-dvr")

	// Published before the window procedure can be asked to restore it.
	current = t

	if err := t.add(); err != nil {
		return err
	}
	return nil
}

// add puts the icon in the notification area.
func (t *tray) add() error {
	if ok, _, err := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&t.icon))); ok == 0 {
		return fmt.Errorf("adding the notification area icon: %w", err)
	}
	return nil
}

// balloon shows one notification over the icon.
func (t *tray) balloon(title, body string) error {
	// Shell_NotifyIcon may be called from any thread; only the window's
	// own queue is thread-bound, and nothing here touches it.
	data := t.icon
	data.uFlags = nifInfo
	data.dwInfoFlags = niifInfo
	data.uVersion = trayTimeout
	copyUTF16(data.szInfoTitle[:], title)
	copyUTF16(data.szInfo[:], body)

	if ok, _, err := procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data))); ok == 0 {
		return fmt.Errorf("showing a balloon: %w", err)
	}
	return nil
}

// close removes the icon and ends the message loop.
func (t *tray) close() (err error) {
	t.closeOnce.Do(func() {
		if ok, _, callErr := procShellNotifyIcon.Call(
			nimDelete, uintptr(unsafe.Pointer(&t.icon))); ok == 0 {
			err = fmt.Errorf("removing the notification area icon: %w", callErr)
		}
		// Posted rather than called: DestroyWindow only works from the
		// thread that made the window, which is the one inside pump.
		procPostMessage.Call(uintptr(t.window), wmClose, 0, 0) //nolint:errcheck // the loop ends on its own if this cannot be posted
		<-t.stopped
	})
	return err
}

// ///////////////////////////////////////////////
// Window procedure
// ///////////////////////////////////////////////

// windowProc serves the hidden window.
//
// It runs on the message loop's thread as a C callback, so it does the
// least possible and never blocks: a Go send from here would park a thread
// Windows is waiting on.
func windowProc(window windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch {
	case msg == wmClose:
		procDestroyWindow.Call(uintptr(window)) //nolint:errcheck // the loop ends either way
		return 0

	case msg == wmDestroy:
		procPostQuitMessage.Call(0) //nolint:errcheck // no failure is defined for it
		return 0

	case taskbarCreated != 0 && msg == taskbarCreated:
		// Explorer restarted and took every icon with it.
		if current != nil {
			current.add() //nolint:errcheck // nothing here can report it, and the next restart tries again
		}
		return 0
	}

	result, _, _ := procDefWindowProc.Call(uintptr(window), uintptr(msg), wParam, lParam)
	return result
}

// ///////////////////////////////////////////////
// Text
// ///////////////////////////////////////////////

// copyUTF16 writes text into a fixed operating-system field.
//
// Every such field is a counted array ending in a NUL, so the last unit is
// left as one and the text is cut at whatever fits. The cut is on a UTF-16
// unit, which can split a surrogate pair and leave an unpaired half that
// renders as a replacement character. Callers clip on a rune bound well
// under these sizes, so this bound is the one that never fires.
func copyUTF16(field []uint16, text string) {
	encoded := windows.StringToUTF16(text)
	if len(encoded) > len(field) {
		encoded = encoded[:len(field)]
		encoded[len(encoded)-1] = 0
	}
	copy(field, encoded)
}
