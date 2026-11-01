// 联动动作：全局快捷键、休眠键、仅息屏后守护、托盘
package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// defaultHotkeys 默认快捷键
func defaultHotkeys() []HotkeyBinding {
	return []HotkeyBinding{
		{Action: "sleep", Mod: modAlt | modControl, VK: 'S', Enabled: true},
		{Action: "screenoff", Mod: modAlt | modControl, VK: 'D', Enabled: true},
		{Action: "screenon", Mod: modAlt | modControl, VK: 'W', Enabled: true},
		{Action: "status", Mod: modAlt | modControl, VK: 'Q', Enabled: true},
		{Action: "hide", Mod: modAlt | modControl, VK: 'H', Enabled: true},
	}
}

// builtinHotkey 内置默认快捷键（旧配置缺失某动作时使用）
func builtinHotkey(action string) HotkeyBinding {
	for _, b := range defaultHotkeys() {
		if b.Action == action {
			return b
		}
	}
	switch action {
	case "show":
		return HotkeyBinding{Action: "show", Mod: modAlt | modControl, VK: 'R', Enabled: true}
	case "hide":
		return HotkeyBinding{Action: "hide", Mod: modAlt | modControl, VK: 'H', Enabled: true}
	}
	return HotkeyBinding{Action: action, Mod: modAlt | modControl, Enabled: true}
}

func hotkeyActionName(action string) string {
	switch action {
	case "sleep":
		return "一键息屏（息屏+休眠）"
	case "screenoff":
		return "仅息屏"
	case "screenon":
		return "亮屏"
	case "status":
		return "查询屏幕状态"
	case "show":
		return "显示主窗口"
	case "hide":
		return "隐藏窗口/后台（再按固定呼出键恢复）"
	}
	if isCustomAction(action) {
		return "自定义命令:" + customActionName(action)
	}
	return action
}

func hotkeyText(b HotkeyBinding) string {
	var s []string
	if b.Mod&modControl != 0 {
		s = append(s, "Ctrl")
	}
	if b.Mod&modAlt != 0 {
		s = append(s, "Alt")
	}
	if b.Mod&modShift != 0 {
		s = append(s, "Shift")
	}
	if b.VK >= 'A' && b.VK <= 'Z' {
		s = append(s, string(rune(b.VK)))
	} else if b.VK >= '0' && b.VK <= '9' {
		s = append(s, string(rune(b.VK)))
	} else if b.VK >= 0x70 && b.VK <= 0x87 {
		s = append(s, "F"+strconv.Itoa(int(b.VK-0x6F)))
	}
	return strings.Join(s, "+")
}

func (a *App) runHotkeyAction(action string) {
	switch action {
	case "sleep":
		a.logCtrl("热键：一键息屏")
		a.actionOneClickSleep()
	case "screenoff":
		a.logCtrl("热键：仅息屏")
		a.actionScreenOnly()
	case "screenon":
		a.logCtrl("热键：亮屏")
		a.actionScreenOn()
	case "status":
		a.logCtrl("热键：查询屏幕状态")
		a.actionQueryStatus()
	case "show":
		a.logCtrl("热键：显示主窗口")
		a.showMainWindow()
	case "hide":
		a.logCtrl("热键：隐藏窗口")
		a.toggleMainWindow()
	}
	if isCustomAction(action) {
		a.logCtrl("热键：自定义命令[%s]", customActionName(action))
		a.launchCustomByName(customActionName(action))
	}
}

// showMainWindow 从托盘/后台恢复主窗口（可在任意线程/热键调用）
func (a *App) showMainWindow() {
	a.mw.Synchronize(func() {
		a.mw.Show()
		if a.tray != nil {
			_ = a.tray.SetVisible(false)
		}
	})
}

// toggleMainWindow 显示↔隐藏切换（热键调用）。隐藏时按“完全后台”设置决定是否连托盘一起隐藏
func (a *App) toggleMainWindow() {
	a.mw.Synchronize(func() {
		if a.mw.Visible() {
			a.mu.Lock()
			bg := a.cfg.Link.Background
			a.mu.Unlock()
			a.hideToTray(bg)
		} else {
			a.showMainWindow()
		}
	})
}

// hideToTray 隐藏窗口并显示托盘（background=true 时连托盘一起隐藏）
func (a *App) hideToTray(background bool) {
	a.mw.Hide()
	if a.tray != nil {
		_ = a.tray.SetVisible(!background)
	}
}

// buildTray 创建托盘图标与完整功能菜单
func (a *App) buildTray() {
	ni, err := walk.NewNotifyIcon(a.mw)
	if err != nil {
		a.logCtrl("创建托盘图标失败: %v", err)
		return
	}
	a.tray = ni
	_ = ni.SetIcon(walk.IconApplication())
	_ = ni.SetToolTip("海信一体机助手")

	m := ni.ContextMenu()
	act := func(text string, fn func()) {
		ac := walk.NewAction()
		_ = ac.SetText(text)
		ac.Triggered().Attach(fn)
		_ = m.Actions().Add(ac)
	}
	sep := func() { _ = m.Actions().Add(walk.NewSeparatorAction()) }
	sub := func(text string) *walk.Menu {
		sm, _ := walk.NewMenu()
		sa, _ := m.Actions().AddMenu(sm)
		_ = sa.SetText(text)
		return sm
	}
	subAct := func(sm *walk.Menu, text string, fn func()) {
		ac := walk.NewAction()
		_ = ac.SetText(text)
		ac.Triggered().Attach(fn)
		_ = sm.Actions().Add(ac)
	}

	act("显示主窗口", a.showMainWindow)
	act("隐藏到托盘", func() { a.hideToTray(false) })
	sep()
	act("一键息屏（息屏+休眠）", a.actionOneClickSleep)
	act("仅息屏", a.actionScreenOnly)
	act("亮屏", a.actionScreenOn)
	sep()

	// 电源
	pw := sub("电源")
	subAct(pw, "整机关机", func() {
		if walk.MsgBox(a.mw, "确认", "确定立即整机关机？", walk.MsgBoxIconQuestion|walk.MsgBoxYesNo) == walk.DlgCmdYes {
			a.schedSendFrame(FramePowerOff)
		}
	})
	subAct(pw, "开机", func() { a.schedSendFrame(FramePowerOn) })
	subAct(pw, "整机重启", func() {
		if walk.MsgBox(a.mw, "确认", "确定立即整机重启？", walk.MsgBoxIconQuestion|walk.MsgBoxYesNo) == walk.DlgCmdYes {
			a.schedSendFrame(FrameReboot)
		}
	})

	// 查询
	qy := sub("状态查询")
	subAct(qy, "屏幕状态", a.actionQueryStatus)
	subAct(qy, "是否开机", func() { a.trayQuery("是否开机", FrameIsOpen) })
	subAct(qy, "软件版本", func() { a.trayQuery("软件版本", FrameVersion) })
	subAct(qy, "整机ID", func() { a.trayQuery("整机ID", FrameGetID) })
	subAct(qy, "当前信源", func() { a.trayQuery("当前信源", FrameSignalNow) })

	// 信源
	sg := sub("信号源切换")
	for _, s := range []struct{ n, k string }{
		{"OPS", "OPS"}, {"HDMI1", "HDMI1"}, {"HDMI2", "HDMI2"},
		{"VGA1", "VGA1"}, {"AV1", "AV1"}, {"YPbPr", "YPbPr"},
	} {
		kind := s.k
		subAct(sg, s.n, func() { a.schedSendFrame(frameSignalSource(kind)) })
	}

	// 图像模式
	md := sub("图像模式")
	for _, mm := range []struct {
		n string
		b byte
	}{{"标准", 0x07}, {"运动", 0x08}, {"录音", 0x09}, {"电影", 0x0A}, {"游戏", 0x0B}} {
		b := mm.b
		subAct(md, mm.n, func() { a.schedSendFrame(frameScreenMode(b)) })
	}

	sep()
	a.mu.Lock()
	hideTrayCmd := a.cfg.Link.HideTrayCustomCmd
	a.mu.Unlock()
	a.addCustomMenu(m, hideTrayCmd)
	sep()
	act("高级隐藏设置…", a.showHideSettingsDialog)
	act("退出程序", func() {
		_ = ni.SetVisible(false)
		_ = ni.Dispose()
		walk.App().Exit(0)
	})

	// 左键单击显示主窗口
	ni.MouseDown().Attach(func(x, y int, b walk.MouseButton) {
		if b == walk.LeftButton {
			a.showMainWindow()
		}
	})
}

// trayQuery 托盘菜单里的查询（串口直发，无界面弹窗）
func (a *App) trayQuery(name, frame string) {
	port := a.ctrlPort()
	if port == "" {
		a.mu.Lock()
		port = a.cfg.Engine.FallbackSerial.COM
		a.mu.Unlock()
	}
	if port == "" {
		a.logCtrl("托盘查询[%s]：未配置串口", name)
		return
	}
	go func() {
		resp, err := serialSend(port, frame)
		if err != nil {
			a.logCtrl("托盘查询[%s]失败: %v", name, err)
			return
		}
		a.logCtrl("托盘查询[%s]回执: %s", name, resp)
	}()
}


// 一键息屏：息屏 + PC 睡眠（串口等阻塞操作放后台，避免卡住界面/托盘菜单）
func (a *App) actionOneClickSleep() {
	go func() {
		a.schedSendFrame(FrameScreenOff)
		time.Sleep(300 * time.Millisecond)
		a.managedSleep("一键息屏")
		a.logCtrl("已触发：息屏 + PC 睡眠")
	}()
}

// 仅息屏：仅关显示，并按开关启动“未休眠则休眠”守护
func (a *App) actionScreenOnly() {
	go func() {
		a.schedSendFrame(FrameScreenOff)
		a.logCtrl("已发送：仅息屏")
		a.startScreenOffGuard()
	}()
}

// 亮屏
func (a *App) actionScreenOn() {
	go func() {
		a.schedSendFrame(FrameScreenOn)
		a.logCtrl("已发送：亮屏")
	}()
}

// 查询屏幕状态（热键用）
func (a *App) actionQueryStatus() {
	go func() {
		switch a.queryScreenStatus() {
		case "on":
			a.logCtrl("屏幕状态：亮屏")
		case "off":
			a.logCtrl("屏幕状态：息屏")
		default:
			a.logCtrl("屏幕状态：未能判定（串口不可用/无回执）")
		}
	}()
}

// queryScreenStatus 返回 "on"/"off"/"unknown"
func (a *App) queryScreenStatus() string {
	port := a.ctrlPort()
	if port == "" {
		a.mu.Lock()
		port = a.cfg.Engine.FallbackSerial.COM
		a.mu.Unlock()
	}
	if port == "" {
		return "unknown"
	}
	resp, err := serialSend(port, FrameStatus)
	if err != nil {
		return "unknown"
	}
	switch {
	case strings.Contains(resp, "01 F4"):
		return "on"
	case strings.Contains(resp, "00 F5"):
		return "off"
	}
	return "unknown"
}

// onSleepKey 按下 PC 休眠键：block=true 仅息屏（拦截休眠），false 息屏后继续系统休眠
func (a *App) onSleepKey(block bool) {
	a.schedSendFrame(FrameScreenOff)
	if block {
		a.logCtrl("检测到休眠键：已自动息屏（并拦截系统休眠，仅息屏）")
	} else {
		a.logCtrl("检测到休眠键：已自动息屏，系统继续休眠")
	}
}

// startScreenOffGuard 启动息屏守护（单实例）：定时查询屏幕状态，
// 查到已息屏但 PC 未休眠（且无人操作、无应用拦截）则自动休眠。
func (a *App) startScreenOffGuard() {
	a.mu.Lock()
	enabled := a.cfg.Link.GuardSleep
	interval := a.cfg.Link.GuardWaitSec
	a.mu.Unlock()
	if !enabled || interval <= 0 {
		return
	}
	a.guardMu.Lock()
	if a.guarding {
		a.guardMu.Unlock()
		return
	}
	a.guarding = true
	a.guardMu.Unlock()

	go func() {
		defer func() {
			a.guardMu.Lock()
			a.guarding = false
			a.guardMu.Unlock()
		}()
		a.logCtrl("息屏守护已启动：每 %d 秒查询屏幕状态，已息屏但 PC 未休眠则自动休眠", interval)
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			// 守护被关闭则退出
			a.mu.Lock()
			on := a.cfg.Link.GuardSleep
			a.mu.Unlock()
			if !on {
				a.logCtrl("息屏守护已关闭，退出监测")
				return
			}
			// 最近 8 秒内有键鼠输入，说明有人在用，本轮跳过
			if idleMillis() < 8000 {
				continue
			}
			if a.queryScreenStatus() != "off" {
				continue // 屏幕仍亮或状态未知，不动作
			}
			a.logCtrl("息屏守护：整机已息屏但 PC 仍未休眠，执行自动休眠")
			// managedSleep 内部处理“应用拦截”与“阻止键鼠唤醒”选项
			a.managedSleep("息屏守护")
		}
	}()
}
