// 隐藏设置：一个隐蔽的高级设置面板（托盘右键“高级隐藏设置…”触发）
// 用于开关一些“隐藏选项”，避免主界面堆太多开关。
package main

import (
	"github.com/lxn/walk"
)

func (a *App) showHideSettingsDialog() {
	a.mw.Synchronize(func() {
		dlg, err := walk.NewDialog(a.mw)
		if err != nil {
			return
		}
		dlg.SetTitle("高级隐藏设置")
		dlg.SetLayout(walk.NewVBoxLayout())

		a.mu.Lock()
		guard := a.cfg.Link.GuardSleep
		hideHelp := a.cfg.Link.HideCmdHelp
		hideTimer := a.cfg.Link.HideManualTimer
		hideTrayCmd := a.cfg.Link.HideTrayCustomCmd
		hideWB := a.cfg.Link.HideWakeBlock
		a.mu.Unlock()

		guardCk, _ := walk.NewCheckBox(dlg)
		guardCk.SetText("息屏守护（全局启动：整机息屏后未休眠则自动休眠 PC）")
		guardCk.SetChecked(guard)
		hideHelpCk, _ := walk.NewCheckBox(dlg)
		hideHelpCk.SetText("隐藏“个性化与工具”页的命令行说明区")
		hideHelpCk.SetChecked(hideHelp)
		hideTimerCk, _ := walk.NewCheckBox(dlg)
		hideTimerCk.SetText("隐藏手动菜单中的“定时写入”项（精简菜单）")
		hideTimerCk.SetChecked(hideTimer)
		hideTrayCk, _ := walk.NewCheckBox(dlg)
		hideTrayCk.SetText("隐藏托盘菜单中的“自定义命令”子菜单")
		hideTrayCk.SetChecked(hideTrayCmd)
		hideWBCk, _ := walk.NewCheckBox(dlg)
		hideWBCk.SetText("隐藏联动页中的“阻止键鼠唤醒”选项")
		hideWBCk.SetChecked(hideWB)

		tip, _ := walk.NewLabel(dlg)
		tip.SetText("注：勾选项在“确定”后保存；除守护开关即时生效外，其余需重启程序后完全生效。")

		btnRow, _ := walk.NewComposite(dlg)
		btnRow.SetLayout(walk.NewHBoxLayout())
		okBtn, _ := walk.NewPushButton(btnRow)
		okBtn.SetText("确定")
		cancelBtn, _ := walk.NewPushButton(btnRow)
		cancelBtn.SetText("取消")
		okBtn.Clicked().Attach(func() {
			a.mu.Lock()
			a.cfg.Link.GuardSleep = guardCk.Checked()
			a.cfg.Link.HideCmdHelp = hideHelpCk.Checked()
			a.cfg.Link.HideManualTimer = hideTimerCk.Checked()
			a.cfg.Link.HideTrayCustomCmd = hideTrayCk.Checked()
			a.cfg.Link.HideWakeBlock = hideWBCk.Checked()
			a.mu.Unlock()
			_ = a.saveConfig()
			// 守护开关即时生效
			if guardCk.Checked() {
				a.startScreenOffGuard()
			}
			dlg.Close(walk.DlgCmdOK)
		})
		cancelBtn.Clicked().Attach(func() { dlg.Close(walk.DlgCmdCancel) })

		dlg.SetMinMaxSize(walk.Size{Width: 430, Height: 0}, walk.Size{Width: 700, Height: 0})
		dlg.Run()
	})
}
