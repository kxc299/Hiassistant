// 电源管理：powercfg 封装（应用拦截休眠检测、键鼠唤醒控制）与统一睡眠入口
package main

import (
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
)

// runHidden 隐藏窗口执行外部命令并返回输出
func runHidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// powerRequestsBlocked 检测是否有应用正在请求系统保持运行（音乐/视频播放等）
// powercfg /requests 输出中 DISPLAY/SYSTEM 段落下若存在 [PROCESS]/[SERVICE]/[DRIVER] 条目即视为被占用
func powerRequestsBlocked() bool {
	out, err := runHidden("powercfg", "/requests")
	if err != nil {
		// 无法判断时不阻断原有流程
		return false
	}
	section := ""
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		upper := strings.ToUpper(line)
		if strings.HasSuffix(upper, ":") {
			switch strings.TrimSuffix(upper, ":") {
			case "DISPLAY", "SYSTEM":
				section = "busy"
			default:
				section = ""
			}
			continue
		}
		if section == "busy" && line != "" && strings.HasPrefix(line, "[") {
			return true
		}
	}
	return false
}

// wakeArmedDevices 列出当前允许唤醒计算机的设备
func wakeArmedDevices() []string {
	out, err := runHidden("powercfg", "/devicequery", "wake_armed")
	if err != nil {
		return nil
	}
	var devs []string
	for _, line := range strings.Split(out, "\n") {
		if d := strings.TrimSpace(line); d != "" {
			devs = append(devs, d)
		}
	}
	return devs
}

// isKeyboardOrMouse 判断设备名是否为键盘/鼠标（含中英文 HID 设备名）
func isKeyboardOrMouse(name string) bool {
	low := strings.ToLower(name)
	for _, kw := range []string{"键盘", "鼠标", "keyboard", "mouse", "hid"} {
		if strings.Contains(low, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

func disableWakeDevice(name string) error {
	_, err := runHidden("powercfg", "/devicedisablewake", name)
	return err
}

func enableWakeDevice(name string) error {
	_, err := runHidden("powercfg", "/deviceenablewake", name)
	return err
}

// managedSleep 统一的 PC 睡眠入口（所有界面/热键/守护调用都走这里）
// 1) 开启“应用拦截休眠”且检测到占用时跳过；
// 2) 开启“阻止键鼠唤醒”时临时禁用键鼠唤醒，恢复后自动还原。
func (a *App) managedSleep(reason string) {
	a.mu.Lock()
	appBlock := a.cfg.Link.GuardAppBlock
	wakeBlock := a.cfg.Link.WakeBlock
	a.mu.Unlock()

	if appBlock && powerRequestsBlocked() {
		a.logCtrl("检测到应用正在阻止休眠（如播放音乐/视频），本次不进行休眠（%s）", reason)
		return
	}

	if wakeBlock {
		var blocked []string
		var failed []string
		for _, d := range wakeArmedDevices() {
			if !isKeyboardOrMouse(d) {
				continue
			}
			if err := disableWakeDevice(d); err != nil {
				failed = append(failed, d)
				continue
			}
			blocked = append(blocked, d)
		}
		if len(blocked) > 0 {
			a.logCtrl("已临时禁用 %d 个键鼠设备的唤醒功能（系统恢复后自动还原）", len(blocked))
		}
		if len(failed) > 0 {
			a.logCtrl("阻止键鼠唤醒：%d 个设备设置失败（powercfg 可能需要管理员权限）", len(failed))
		}
		a.mu.Lock()
		a.wakeBlocked = blocked
		a.mu.Unlock()
	}

	sleepPC()
}

// restoreWakeBlock 系统恢复后重新启用此前被禁用的键鼠唤醒
func (a *App) restoreWakeBlock() {
	a.mu.Lock()
	blocked := a.wakeBlocked
	a.wakeBlocked = nil
	a.mu.Unlock()
	if len(blocked) == 0 {
		return
	}
	var failed int
	for _, d := range blocked {
		if err := enableWakeDevice(d); err != nil {
			failed++
		}
	}
	if failed > 0 {
		a.logCtrl("恢复键鼠唤醒：%d 个设备失败（可能需要管理员权限）", failed)
	} else {
		a.logCtrl("已恢复 %d 个键鼠设备的唤醒功能", len(blocked))
	}
}
