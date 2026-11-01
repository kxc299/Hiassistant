// 全局快捷键、休眠键监听、仅息屏后守护（Win32 实现，CGO_ENABLED=0）
package main

import (
	"runtime"
	"syscall"
	"unsafe"

	"github.com/lxn/win"
)

var (
	user32 = syscall.NewLazyDLL("user32.dll")
	kern32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterHotKey      = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey    = user32.NewProc("UnregisterHotKey")
	procSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procGetLastInputInfo    = user32.NewProc("GetLastInputInfo")
	procGetTickCount        = kern32.NewProc("GetTickCount")
)

const (
	whKeyboardLL = 13
	hcAction     = 0
	wmKeydown    = 0x0100
	wmSysKeydown = 0x0104
	wmHotkey     = 0x0312
	wmPowerBcast = 0x0218
	pbtSuspend   = 0x0004  // PBT_APMSUSPEND：系统即将进入睡眠
	pbtResumeAuto = 0x0012 // PBT_APMRESUMEAUTOMATIC：系统已自动恢复
	pbtResumeUser = 0x0007 // PBT_APMRESUMESUSPEND
	wmAppReapply = 0x0400 + 1
	vkSleep      = 0x5F

	wmQueryEndSession = 0x0011 // Windows 询问是否允许关机/注销
	wmEndSession      = 0x0016 // 确认关机/注销
	endSessionLogoff  = 0x80000000 // ENDSESSION_LOGOFF：本次结束会话是“注销”

	modAlt      = 0x0001
	modControl  = 0x0002
	modShift    = 0x0004
	modNoRepeat = 0x4000

	hkShowWindow = uintptr(99) // 固定的“呼出主窗口”热键，后台模式下也可恢复
	hotkeyClass  = "HisenseAssistantHotkeyCls"
)

type kbdLLHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type lastInputInfo struct {
	CbSize uint32
	DwTime uint32
}

type hotkeyHub struct {
	app          *App
	hwnd         win.HWND
	hook         uintptr
	wndCb        uintptr
	keyCb        uintptr
	shutdownSent bool
	shutdownSkip bool
}

func newHotkeyHub(app *App) *hotkeyHub {
	return &hotkeyHub{app: app}
}

// run 在独立 OS 线程跑消息循环（热键/钩子都要求该线程在消息泵上）
func (h *hotkeyHub) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	h.wndCb = syscall.NewCallback(h.wndProc)
	h.keyCb = syscall.NewCallback(h.keyboardProc)

	hInst := win.GetModuleHandle(nil)
	className, _ := syscall.UTF16PtrFromString(hotkeyClass)
	wc := win.WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
		Style:         win.CS_HREDRAW | win.CS_VREDRAW,
		LpfnWndProc:   h.wndCb,
		HInstance:     hInst,
		LpszClassName: className,
	}
	if atom := win.RegisterClassEx(&wc); atom == 0 {
		h.app.logCtrl("注册热键窗口类失败")
		return
	}
	// 顶级隐藏窗口（不能用 HWND_MESSAGE 消息窗口，否则收不到 WM_POWERBROADCAST 系统电源广播）
	h.hwnd = win.CreateWindowEx(
		0, className, className, win.WS_OVERLAPPED,
		0, 0, 0, 0,
		0, 0, hInst, nil)
	if h.hwnd == 0 {
		h.app.logCtrl("创建热键窗口失败")
		return
	}

	hook, _, _ := procSetWindowsHookEx.Call(
		uintptr(whKeyboardLL), h.keyCb, uintptr(hInst), 0)
	if hook == 0 {
		h.app.logCtrl("安装键盘钩子失败（可能被安全软件拦截）")
	} else {
		h.hook = hook
	}

	h.applyHotkeys()

	var msg win.MSG
	for win.GetMessage(&msg, 0, 0, 0) > 0 {
		win.TranslateMessage(&msg)
		win.DispatchMessage(&msg)
	}
}

func (h *hotkeyHub) wndProc(hwnd win.HWND, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmAppReapply:
		h.applyHotkeys()
		return 0
	case wmHotkey:
		h.onHotkey(wParam)
		return 0
	case wmPowerBcast:
		h.onPowerBroadcast(uint32(wParam))
		return 1 // 电源广播返回 TRUE
	case wmQueryEndSession:
		// 允许关机；QUERY 阶段系统已确定要结束会话，此时读取注入标志并联动成功率最高
		h.onWindowsShutdown(lParam&endSessionLogoff != 0)
		return 1
	case wmEndSession:
		if wParam == 1 {
			h.onWindowsShutdown(lParam&endSessionLogoff != 0)
		}
		return 0
	}
	return win.DefWindowProc(hwnd, message, wParam, lParam)
}

// ==================== Windows 关机/重启 联动（纯 Go，无注入） ====================
// Windows 的 WM_QUERYENDSESSION / WM_ENDSESSION 不区分“关机”还是“重启”，两者走同一
// 结束会话流程，且消息参数不含关机类型；纯 Go（无 DLL 注入）无法在关机瞬间可靠区分。
// 因此本版逻辑：
//   - 注销（ENDSESSION_LOGOFF 位置位）：系统仍继续运行，精确排除，不联动；
//   - 关机 / 重启（非注销）：统一视为“系统结束会话”，联动关闭整机。
// 若不希望重启时联动，可在“联动设置”里关闭本开关。

// onWindowsShutdown 检测到 Windows 结束会话（只处理一次）。
// isLogoff=true 表示本次为“注销”：系统仍继续运行，不关整机，不联动。
// 非注销（关机/重启）：消息层无法区分，统一联动关闭整机。
func (h *hotkeyHub) onWindowsShutdown(isLogoff bool) {
	if h.shutdownSent || h.shutdownSkip {
		return
	}
	if isLogoff {
		h.shutdownSkip = true
		h.app.logCtrl("检测到 Windows 注销，不联动整机关机")
		return
	}
	h.app.mu.Lock()
	enabled := h.app.cfg.Link.WinShutdownLink
	h.app.mu.Unlock()
	if !enabled {
		h.shutdownSkip = true
		h.app.logCtrl("检测到 Windows 结束会话，但“关机联动”未启用，跳过")
		return
	}
	h.doPowerOff("Windows 结束会话（关机或重启，消息层不区分；注销已排除）")
}

func (h *hotkeyHub) doPowerOff(reason string) {
	if h.shutdownSent {
		return
	}
	h.shutdownSent = true
	h.app.logCtrl("检测到 Windows 真实关机：%s，自动关闭整机…", reason)
	if err := serialWrite(h.app.schedPort(), FramePowerOff); err != nil {
		h.app.logCtrl("关机联动：整机关机帧下发失败: %v", err)
	} else {
		h.app.logCtrl("关机联动：整机关机帧已下发")
	}
}

// onPowerBroadcast 系统全局休眠/恢复事件（开始菜单睡眠、合盖、电源键等）
func (h *hotkeyHub) onPowerBroadcast(event uint32) {
	switch event {
	case pbtSuspend:
		h.app.mu.Lock()
		enabled := h.app.cfg.Link.SleepEventScreenOff
		h.app.mu.Unlock()
		if enabled {
			// 系统即将睡眠，线程内同步尽快下发息屏帧（不弹任何界面）
			h.app.schedSendFrame(FrameScreenOff)
			h.app.logCtrl("检测到系统休眠事件（开始菜单睡眠/合盖/电源键）：已自动息屏")
		}
	case pbtResumeAuto, pbtResumeUser:
		h.app.logCtrl("系统已从睡眠中恢复")
		h.app.restoreWakeBlock()
	}
}

func (h *hotkeyHub) applyHotkeys() {
	// 先清理所有旧注册
	for id := uintptr(1); id <= 16; id++ {
		procUnregisterHotKey.Call(uintptr(h.hwnd), id)
	}
	procUnregisterHotKey.Call(uintptr(h.hwnd), hkShowWindow)

	// 固定的“呼出主窗口”热键（完全后台模式下用于恢复）
	procRegisterHotKey.Call(uintptr(h.hwnd), hkShowWindow,
		uintptr(modAlt|modControl|modShift|modNoRepeat), 'M')

	h.app.mu.Lock()
	enabled := h.app.cfg.Link.HotkeysEnabled
	list := make([]HotkeyBinding, len(h.app.cfg.Link.Hotkeys))
	copy(list, h.app.cfg.Link.Hotkeys)
	h.app.mu.Unlock()
	if !enabled {
		return
	}

	// 防冲突：检查组内重复组合
	seen := map[uint64]string{}
	for i := range list {
		b := &list[i]
		if !b.Enabled || b.VK == 0 {
			continue
		}
		key := uint64(b.Mod&0x07)<<32 | uint64(b.VK)
		if _, dup := seen[key]; dup {
			h.app.logCtrl("快捷键冲突：动作[%s]与其它快捷键相同，已跳过", hotkeyActionName(b.Action))
			continue
		}
		seen[key] = hotkeyActionName(b.Action)
		id := uintptr(i + 1)
		r, _, err := procRegisterHotKey.Call(
			uintptr(h.hwnd), id,
			uintptr(b.Mod&0x07|modNoRepeat), uintptr(b.VK))
		if r == 0 {
			h.app.logCtrl("快捷键注册失败：%s（%s），可能被其他软件占用: %v",
				hotkeyActionName(b.Action), hotkeyText(*b), err)
		} else {
			h.app.logCtrl("快捷键已注册：%s = %s", hotkeyActionName(b.Action), hotkeyText(*b))
		}
	}
}

// requestApply 让热键重注册发生在窗口线程
func (h *hotkeyHub) requestApply() {
	if h.hwnd != 0 {
		win.PostMessage(h.hwnd, wmAppReapply, 0, 0)
	}
}

func (h *hotkeyHub) onHotkey(id uintptr) {
	if id == hkShowWindow {
		// 固定呼出键：显示↔隐藏切换，后台/托盘都能恢复
		h.app.toggleMainWindow()
		return
	}
	h.app.mu.Lock()
	idx := int(id) - 1
	var action string
	if idx >= 0 && idx < len(h.app.cfg.Link.Hotkeys) {
		action = h.app.cfg.Link.Hotkeys[idx].Action
	}
	h.app.mu.Unlock()
	// 热键动作含串口/休眠等阻塞操作，放到 goroutine，避免卡住消息泵
	go h.app.runHotkeyAction(action)
}

// keyboardProc 低级键盘钩子：捕获休眠键
func (h *hotkeyHub) keyboardProc(nCode int, wParam, lParam uintptr) uintptr {
	if nCode == hcAction && (wParam == wmKeydown || wParam == wmSysKeydown) {
		k := (*kbdLLHookStruct)(unsafe.Pointer(lParam))
		if k.VkCode == vkSleep {
			h.app.mu.Lock()
			enabled := h.app.cfg.Link.SleepKeyScreenOff
			block := h.app.cfg.Link.SleepKeyBlockSleep
			h.app.mu.Unlock()
			if enabled {
				block := block
				go h.app.onSleepKey(block) // 串口等阻塞操作放后台，避免卡住全局键盘钩子
				if block {
					return 1 // 吞掉按键，阻止系统休眠
				}
			}
		}
	}
	r, _, _ := procCallNextHookEx.Call(h.hook, uintptr(nCode), wParam, lParam)
	return r
}

// idleMillis 返回距最近一次键鼠输入的毫秒数
func idleMillis() uint32 {
	var li lastInputInfo
	li.CbSize = uint32(unsafe.Sizeof(li))
	tick, _, _ := procGetTickCount.Call()
	if r, _, _ := syscall.Syscall(procGetLastInputInfo.Addr(), 1, uintptr(unsafe.Pointer(&li)), 0, 0); r != 0 {
		return uint32(tick) - li.DwTime
	}
	return 0
}
