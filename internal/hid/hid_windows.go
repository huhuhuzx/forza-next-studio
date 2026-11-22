//go:build windows

package hid

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var (
	modSetupAPI = syscall.NewLazyDLL("setupapi.dll")
	modHID      = syscall.NewLazyDLL("hid.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modWinMM    = syscall.NewLazyDLL("winmm.dll")

	procHidDGetHidGuid         = modHID.NewProc("HidD_GetHidGuid")
	procHidDGetAttributes      = modHID.NewProc("HidD_GetAttributes")
	procHidDGetProductString   = modHID.NewProc("HidD_GetProductString")
	procHidDGetPreparsedData   = modHID.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsedData  = modHID.NewProc("HidD_FreePreparsedData")
	procHidPGetCaps            = modHID.NewProc("HidP_GetCaps")
	procSetupDiGetClassDevsW   = modSetupAPI.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDevIfaces   = modSetupAPI.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDevIfaceDetW = modSetupAPI.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDevInfo  = modSetupAPI.NewProc("SetupDiDestroyDeviceInfoList")
	procTimeBeginPeriod        = modWinMM.NewProc("timeBeginPeriod")
	procTimeEndPeriod          = modWinMM.NewProc("timeEndPeriod")
)

const (
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010
	reportLength33       = 33

	// Known primary X-Haptics / Flydigi 86-series controller VID/PID/UsagePage.
	PreferredVID       uint16 = 0x37D7
	PreferredPID       uint16 = 0x2502
	PreferredUsagePage uint16 = 0xFFA0
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type spDeviceInterfaceData struct {
	CbSize             uint32
	InterfaceClassGuid guid
	Flags              uint32
	Reserved           uintptr
}

type hiddAttributes struct {
	Size          uint32
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
}

type hidpCaps struct {
	Usage                     uint16
	UsagePage                 uint16
	InputReportByteLength     uint16
	OutputReportByteLength    uint16
	FeatureReportByteLength   uint16
	Reserved                  [17]uint16
	NumberLinkCollectionNodes uint16
	NumberInputButtonCaps     uint16
	NumberInputValueCaps      uint16
	NumberInputDataIndices    uint16
	NumberOutputButtonCaps    uint16
	NumberOutputValueCaps     uint16
	NumberOutputDataIndices   uint16
	NumberFeatureButtonCaps   uint16
	NumberFeatureValueCaps    uint16
	NumberFeatureDataIndices  uint16
}

// DeviceInfo describes an enumerable 33-byte HID output device.
type DeviceInfo struct {
	ID          int    `json:"id"`
	VID         string `json:"vid"`
	PID         string `json:"pid"`
	VIDNum      uint16 `json:"-"`
	PIDNum      uint16 `json:"-"`
	UsagePage   string `json:"usagePage"`
	UsagePageN  uint16 `json:"-"`
	OutputBytes int    `json:"outputBytes"`
	Product     string `json:"product"`
	Path        string `json:"path"`
	Preferred   bool   `json:"preferred"`
	Writing     bool   `json:"writing"`
}

// BeginHighResTimer requests 1 ms OS timer resolution on Windows.
func BeginHighResTimer() {
	_, _, _ = procTimeBeginPeriod.Call(1)
}

// EndHighResTimer releases the 1 ms timer resolution request.
func EndHighResTimer() {
	_, _, _ = procTimeEndPeriod.Call(1)
}

// Enumerate33ByteDevices finds all HID interfaces capable of accepting 33-byte output reports.
// By default preferred devices (37d7:2502, usagePage ffa0) are sorted first.
func Enumerate33ByteDevices(includeAll bool) []DeviceInfo {
	var hidGuid guid
	_, _, _ = procHidDGetHidGuid.Call(uintptr(unsafe.Pointer(&hidGuid)))

	hDevInfo, _, _ := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&hidGuid)),
		0,
		0,
		uintptr(digcfPresent|digcfDeviceInterface),
	)
	devices := make([]DeviceInfo, 0)
	if hDevInfo == uintptr(syscall.InvalidHandle) || hDevInfo == 0 {
		return devices
	}
	defer procSetupDiDestroyDevInfo.Call(hDevInfo)

	for idx := uint32(0); ; idx++ {
		var ifaceData spDeviceInterfaceData
		ifaceData.CbSize = uint32(unsafe.Sizeof(ifaceData))
		ok, _, _ := procSetupDiEnumDevIfaces.Call(
			hDevInfo,
			0,
			uintptr(unsafe.Pointer(&hidGuid)),
			uintptr(idx),
			uintptr(unsafe.Pointer(&ifaceData)),
		)
		if ok == 0 {
			break
		}

		var reqSize uint32
		_, _, _ = procSetupDiGetDevIfaceDetW.Call(
			hDevInfo,
			uintptr(unsafe.Pointer(&ifaceData)),
			0,
			0,
			uintptr(unsafe.Pointer(&reqSize)),
			0,
		)
		if reqSize < 8 || reqSize > 4096 {
			continue
		}
		buf := make([]byte, reqSize)
		// SP_DEVICE_INTERFACE_DETAIL_DATA_W.cbSize is 8 on amd64, 6 on 386.
		cbSize := uint32(8)
		if unsafe.Sizeof(uintptr(0)) == 4 {
			cbSize = 6
		}
		*(*uint32)(unsafe.Pointer(&buf[0])) = cbSize

		ok, _, _ = procSetupDiGetDevIfaceDetW.Call(
			hDevInfo,
			uintptr(unsafe.Pointer(&ifaceData)),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(reqSize),
			0,
			0,
		)
		if ok == 0 {
			continue
		}

		path := utf16BytesToString(buf[4:])
		if path == "" {
			continue
		}

		info, valid := inspectHIDPath(path)
		if !valid {
			continue
		}
		if !includeAll && !info.Preferred {
			// Still keep devices on vendor usage pages (>= 0xFF00) with exact 33B output
			if info.UsagePageN < 0xFF00 {
				continue
			}
		}
		devices = append(devices, info)
	}

	sort.SliceStable(devices, func(i, j int) bool {
		if devices[i].Preferred != devices[j].Preferred {
			return devices[i].Preferred
		}
		if devices[i].VIDNum != devices[j].VIDNum {
			return devices[i].VIDNum < devices[j].VIDNum
		}
		return devices[i].Path < devices[j].Path
	})
	for i := range devices {
		devices[i].ID = i
	}
	return devices
}

func inspectHIDPath(path string) (DeviceInfo, bool) {
	pUTF16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return DeviceInfo{}, false
	}
	// Open with 0 access first for safe metadata query; fallback to READ|WRITE if needed.
	h, err := syscall.CreateFile(
		pUTF16,
		0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil || h == syscall.InvalidHandle {
		h, err = syscall.CreateFile(
			pUTF16,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
			nil,
			syscall.OPEN_EXISTING,
			0,
			0,
		)
		if err != nil || h == syscall.InvalidHandle {
			return DeviceInfo{}, false
		}
	}
	defer syscall.CloseHandle(h)

	var attrs hiddAttributes
	attrs.Size = uint32(unsafe.Sizeof(attrs))
	ok, _, _ := procHidDGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&attrs)))
	if ok == 0 {
		return DeviceInfo{}, false
	}

	var preparsed uintptr
	ok, _, _ = procHidDGetPreparsedData.Call(uintptr(h), uintptr(unsafe.Pointer(&preparsed)))
	if ok == 0 || preparsed == 0 {
		return DeviceInfo{}, false
	}
	defer procHidDFreePreparsedData.Call(preparsed)

	var caps hidpCaps
	status, _, _ := procHidPGetCaps.Call(preparsed, uintptr(unsafe.Pointer(&caps)))
	// HIDP_STATUS_SUCCESS == 0x00110000
	if uint32(status) != 0x00110000 {
		return DeviceInfo{}, false
	}
	if caps.OutputReportByteLength != reportLength33 {
		return DeviceInfo{}, false
	}

	var prodBuf [128]uint16
	prod := ""
	ok, _, _ = procHidDGetProductString.Call(uintptr(h), uintptr(unsafe.Pointer(&prodBuf[0])), uintptr(len(prodBuf)*2))
	if ok != 0 {
		prod = syscall.UTF16ToString(prodBuf[:])
	}
	prod = strings.TrimSpace(prod)
	if prod == "" {
		prod = fmt.Sprintf("HID %04x:%04x", attrs.VendorID, attrs.ProductID)
	}

	pref := (attrs.VendorID == PreferredVID && attrs.ProductID == PreferredPID && caps.UsagePage == PreferredUsagePage) ||
		(attrs.VendorID == PreferredVID && caps.UsagePage == PreferredUsagePage)

	return DeviceInfo{
		VID:         fmt.Sprintf("%04x", attrs.VendorID),
		PID:         fmt.Sprintf("%04x", attrs.ProductID),
		VIDNum:      attrs.VendorID,
		PIDNum:      attrs.ProductID,
		UsagePage:   fmt.Sprintf("%04x", caps.UsagePage),
		UsagePageN:  caps.UsagePage,
		OutputBytes: int(caps.OutputReportByteLength),
		Product:     prod,
		Path:        path,
		Preferred:   pref,
	}, true
}

func utf16BytesToString(b []byte) string {
	n := len(b) / 2
	u := make([]uint16, n)
	for i := 0; i < n; i++ {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		if u[i] == 0 {
			u = u[:i]
			break
		}
	}
	return syscall.UTF16ToString(u)
}

// --- 33-Byte Wire Protocol Builders (verified against v4 outputs.jsonl) ---

// Checksum computes sum(frame[3..31]) & 0xFF.
func Checksum(frame *[33]byte) byte {
	var sum uint32
	for i := 3; i <= 31; i++ {
		sum += uint32(frame[i])
	}
	return byte(sum & 0xFF)
}

// BuildCtrl0x53 builds a 33-byte command 0x53 control/init frame.
func BuildCtrl0x53(payload ...byte) [33]byte {
	var f [33]byte
	f[0] = 0x03
	f[1] = 0x5A
	f[2] = 0xA5
	f[3] = 0x53
	f[4] = byte(len(payload) + 2)
	for i := 0; i < len(payload) && 5+i <= 31; i++ {
		f[5+i] = payload[i]
	}
	f[32] = Checksum(&f)
	return f
}

// BuildWave0x57 builds a 33-byte 1 kHz / 8 ms realtime waveform frame.
func BuildWave0x57(flags byte, trig, gl, gr [8]byte) [33]byte {
	var f [33]byte
	f[0] = 0x03
	f[1] = 0x5A
	f[2] = 0xA5
	f[3] = 0x57
	f[4] = 0x1B
	f[5] = flags
	for i := 0; i < 8; i++ {
		f[6+i*3] = trig[i]
		f[7+i*3] = gl[i]
		f[8+i*3] = gr[i]
	}
	f[32] = Checksum(&f)
	return f
}

// BuildNeutral0x57 builds a silent 0x57 frame (flags=0x84, all samples=0x80).
func BuildNeutral0x57() [33]byte {
	var z [8]byte
	for i := range z {
		z[i] = 0x80
	}
	return BuildWave0x57(0x84, z, z, z)
}

// FrameHex returns lowercase 66-char hex string of a 33-byte report.
func FrameHex(f [33]byte) string {
	return hex.EncodeToString(f[:])
}

// Writer manages exclusive write access to one 33-byte HID controller.
type Writer struct {
	mu              sync.Mutex
	handle          syscall.Handle
	activePath      string
	activeDev       *DeviceInfo
	written         uint64
	failures        uint64
	lastErr         string
	rightTrigEnable bool
	initialized     bool
}

func NewWriter() *Writer {
	return &Writer{handle: syscall.InvalidHandle}
}

func (w *Writer) Open(dev DeviceInfo) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.closeLocked()

	pUTF16, err := syscall.UTF16PtrFromString(dev.Path)
	if err != nil {
		w.lastErr = err.Error()
		return err
	}
	h, err := syscall.CreateFile(
		pUTF16,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil || h == syscall.InvalidHandle {
		if err == nil {
			err = errors.New("CreateFile returned invalid handle")
		}
		w.lastErr = fmt.Sprintf("无法打开手柄（可能被 Flydigi Space/Engine 占用）: %v", err)
		return errors.New(w.lastErr)
	}
	w.handle = h
	w.activePath = dev.Path
	dCopy := dev
	dCopy.Writing = true
	w.activeDev = &dCopy
	w.lastErr = ""
	w.rightTrigEnable = false
	w.initialized = false

	// Send verified v4 3-packet 0x53 initialization sequence.
	initFrames := [][33]byte{
		BuildCtrl0x53(0x01, 0x01, 0x03, 0x06, 0x00), // Right trigger off
		BuildCtrl0x53(0x01, 0x00, 0x03, 0x06, 0x40), // Left trigger enable
		BuildCtrl0x53(0x01, 0x12, 0x02, 0x40, 0x00), // Grip/haptic stream enable
		BuildNeutral0x57(),
	}
	for _, fr := range initFrames {
		if err := w.writeLocked(fr); err != nil {
			w.closeLocked()
			return err
		}
	}
	w.initialized = true
	return nil
}

func (w *Writer) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeLocked()
}

func (w *Writer) closeLocked() {
	if w.handle != syscall.InvalidHandle && w.handle != 0 {
		// Send neutral silence + disable triggers before releasing handle.
		_ = w.writeLocked(BuildNeutral0x57())
		_ = w.writeLocked(BuildCtrl0x53(0x01, 0x01, 0x03, 0x06, 0x00))
		_ = w.writeLocked(BuildCtrl0x53(0x01, 0x00, 0x03, 0x06, 0x00))
		_ = syscall.CloseHandle(w.handle)
	}
	w.handle = syscall.InvalidHandle
	w.activePath = ""
	w.activeDev = nil
	w.rightTrigEnable = false
	w.initialized = false
}

func (w *Writer) IsOpen() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.handle != syscall.InvalidHandle && w.handle != 0
}

func (w *Writer) ActivePath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activePath
}

func (w *Writer) Stats() (written, failures uint64, lastErr string, open bool, dev *DeviceInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var cp *DeviceInfo
	if w.activeDev != nil {
		d := *w.activeDev
		cp = &d
	}
	return w.written, w.failures, w.lastErr, w.handle != syscall.InvalidHandle && w.handle != 0, cp
}

// SyncRightTrigger sends 0x53 right trigger enable/disable only when state changes.
func (w *Writer) SyncRightTrigger(enable bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.handle == syscall.InvalidHandle || w.handle == 0 || !w.initialized {
		return
	}
	if enable == w.rightTrigEnable {
		return
	}
	val := byte(0x00)
	if enable {
		val = 0x40
	}
	if err := w.writeLocked(BuildCtrl0x53(0x01, 0x01, 0x03, 0x06, val)); err == nil {
		w.rightTrigEnable = enable
	}
}

func (w *Writer) WriteReport(frame [33]byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.handle == syscall.InvalidHandle || w.handle == 0 {
		return nil
	}
	return w.writeLocked(frame)
}

func (w *Writer) writeLocked(frame [33]byte) error {
	var n uint32
	err := syscall.WriteFile(w.handle, frame[:], &n, nil)
	if err != nil || n != reportLength33 {
		w.failures++
		if err != nil {
			w.lastErr = err.Error()
		} else {
			w.lastErr = fmt.Sprintf("short HID write: %d/33", n)
		}
		return errors.New(w.lastErr)
	}
	w.written++
	return nil
}
