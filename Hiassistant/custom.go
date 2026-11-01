// 第 4 标签页：个性化（皮肤/字体）、自定义外部命令、命令行调用说明
package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/lxn/walk"
)

func newOSCmd(path string, args []string) *exec.Cmd {
	return exec.Command(path, args...)
}

func (a *App) buildPersonalTab(tab *walk.TabPage) {
	tab.SetLayout(newTabPageLayout())
	sv, _ := walk.NewScrollView(tab)
	sv.SetScrollbars(false, true)
	sv.SetLayout(walk.NewVBoxLayout())
	var page walk.Container = sv

	// 顶部预声明：供后续/更早的闭包引用（Go 作用域要求声明点前对闭包可见）
	var (
		opCombo    *walk.ComboBox
		fwCk       *walk.CheckBox
		statusLabel *walk.Label
	)

	// ===== 皮肤与字体 =====
	t1 := boldLabel(page)
	t1.SetText("皮肤与字体")
	skinRow, _ := walk.NewComposite(page)
	skinRow.SetLayout(walk.NewHBoxLayout())
	sl, _ := walk.NewLabel(skinRow)
	sl.SetText("皮肤：")
	skinCombo, _ := walk.NewComboBox(skinRow)
	skins := skinList()
	names := make([]string, len(skins))
	for i, s := range skins {
		names[i] = s.name
	}
	_ = skinCombo.SetModel(names)
	curSkinID := a.cfg.Link.Skin
	for i, s := range skins {
		if s.id == curSkinID {
			_ = skinCombo.SetCurrentIndex(i)
			break
		}
	}
	fl, _ := walk.NewLabel(skinRow)
	fl.SetText("字体大小：")
	fontCombo, _ := walk.NewComboBox(skinRow)
	fontSizes := []string{"9", "10", "11", "12"}
	_ = fontCombo.SetModel(fontSizes)
	curSize := a.cfg.Link.FontSize
	if curSize <= 0 {
		curSize = 9
	}
	for i, v := range fontSizes {
		if v == strconv.Itoa(curSize) {
			_ = fontCombo.SetCurrentIndex(i)
		}
	}
	applyBtn, _ := walk.NewPushButton(skinRow)
	applyBtn.SetText("应用并保存")
	applyBtn.Clicked().Attach(func() {
		si := skinCombo.CurrentIndex()
		if si < 0 || si >= len(skins) {
			si = 0
		}
		id := skins[si].id
		fi := fontCombo.CurrentIndex()
		size := 9
		if fi >= 0 && fi < len(fontSizes) {
			size = atoiSafe(fontSizes[fi])
		}
		op := 100
		if opCombo != nil {
			op = 100 - opCombo.CurrentIndex()*10
		}
		if op < 60 || op > 100 {
			op = 100
		}
		a.mu.Lock()
		a.cfg.Link.Skin = id
		a.cfg.Link.FontSize = size
		a.cfg.Link.Opacity = op
		a.cfg.Link.FullWindowSkin = fwCk != nil && fwCk.Checked()
		a.mu.Unlock()
		if err := a.saveConfig(); err != nil {
			if statusLabel != nil {
				statusLabel.SetText("✗ 保存失败：" + err.Error() + "（请确认 plan.json 可写、程序不在只读压缩包内运行）")
			}
			return
		}
		a.applySkin(id, size)
		if statusLabel != nil {
			statusLabel.SetText(fmt.Sprintf("✓ 已保存并应用：皮肤=%s，字体=%d，透明度=%d%%，全窗口皮肤=%v",
				id, size, op, a.cfg.Link.FullWindowSkin))
		}
	})

	// 自定义图片 / 颜色
	skinRow2, _ := walk.NewComposite(page)
	skinRow2.SetLayout(walk.NewHBoxLayout())
	imgBtn, _ := walk.NewPushButton(skinRow2)
	imgBtn.SetText("自定义背景图片…")
	bgBtn, _ := walk.NewPushButton(skinRow2)
	bgBtn.SetText("背景色…")
	txtBtn, _ := walk.NewPushButton(skinRow2)
	txtBtn.SetText("文字色…")
	rstBtn, _ := walk.NewPushButton(skinRow2)
	rstBtn.SetText("恢复默认")
	imgBtn.Clicked().Attach(func() {
		fd := walk.FileDialog{Title: "选择背景图片", Filter: "图片 (*.png;*.jpg;*.jpeg;*.bmp)|*.png;*.jpg;*.jpeg;*.bmp|所有文件 (*.*)|*.*"}
		if ok, _ := fd.ShowOpen(a.mw); ok && fd.FilePath != "" {
			a.mu.Lock()
			a.cfg.Link.SkinImage = fd.FilePath
			a.mu.Unlock()
			_ = a.saveConfig()
			a.applySkin(a.cfg.Link.Skin, a.cfg.Link.FontSize)
		}
	})
	reapplySkin := func() {
		a.applySkin(a.cfg.Link.Skin, a.cfg.Link.FontSize)
		if statusLabel != nil {
			statusLabel.SetText("✓ 已应用：背景图片/颜色已更新。")
		}
	}
	bgBtn.Clicked().Attach(func() {
		cur := skinByID(a.cfg.Link.Skin).bgColor
		if c, ok := parseHexColor(a.cfg.Link.SkinColorBg); ok {
			cur = c
		}
		nc, ok := a.chooseColorDialog(uintptr(a.mw.Handle()), cur)
		if !ok {
			return
		}
		a.mu.Lock()
		a.cfg.Link.SkinColorBg = colorToHex(nc)
		a.mu.Unlock()
		_ = a.saveConfig()
		reapplySkin()
	})
	txtBtn.Clicked().Attach(func() {
		cur := skinByID(a.cfg.Link.Skin).textColor
		if c, ok := parseHexColor(a.cfg.Link.SkinColorText); ok {
			cur = c
		}
		nc, ok := a.chooseColorDialog(uintptr(a.mw.Handle()), cur)
		if !ok {
			return
		}
		a.mu.Lock()
		a.cfg.Link.SkinColorText = colorToHex(nc)
		a.mu.Unlock()
		_ = a.saveConfig()
		reapplySkin()
	})
	rstBtn.Clicked().Attach(func() {
		a.mu.Lock()
		a.cfg.Link.SkinImage = ""
		a.cfg.Link.SkinColorBg = ""
		a.cfg.Link.SkinColorText = ""
		a.mu.Unlock()
		_ = a.saveConfig()
		reapplySkin()
	})

	// 透明度 / 全窗口皮肤
	skinRow3, _ := walk.NewComposite(page)
	skinRow3.SetLayout(walk.NewHBoxLayout())
	opL, _ := walk.NewLabel(skinRow3)
	opL.SetText("窗口透明度：")
	opCombo, _ = walk.NewComboBox(skinRow3)
	opts := []string{"100%（不透明）", "90%", "80%", "70%", "60%"}
	_ = opCombo.SetModel(opts)
	curOp := a.cfg.Link.Opacity
	if curOp < 60 || curOp > 100 {
		curOp = 100
	}
	_ = opCombo.SetCurrentIndex(100 - curOp) // 100->0, 90->1, 80->2, 70->3, 60->4
	fwCk, _ = walk.NewCheckBox(skinRow3)
	fwCk.SetText("全窗口皮肤（整窗同色，含各标签页背景）")
	fwCk.SetChecked(a.cfg.Link.FullWindowSkin)
	applyBtn.Clicked().Attach(func() {
		// （合并到上方 applyBtn 的单个处理器中，避免重复绑定导致逻辑冲突）
	})

	// 保存结果提示（非模态，避免分层窗口下系统模态框不显示/卡住导致“看似未保存”）
	statusLabel, _ = walk.NewLabel(page)
	statusLabel.SetText("设置已就绪（点击上方按钮保存并应用，结果在此显示）。")

	hideTip, _ := walk.NewLabel(page)
	hideTip.SetText("提示：托盘图标右键菜单可进入“高级隐藏设置”。")

	// ===== 自定义命令（外部程序） =====
	t2 := boldLabel(page)
	t2.SetText("自定义命令（外部程序，可由菜单/托盘/命令行调用）")
	lbRow, _ := walk.NewComposite(page)
	lbRow.SetLayout(walk.NewHBoxLayout())
	listBox, _ := walk.NewListBox(lbRow)
	listBox.SetMinMaxSize(walk.Size{Width: 260, Height: 160}, walk.Size{Width: 400, Height: 300})
	btnCol, _ := walk.NewComposite(lbRow)
	btnCol.SetLayout(walk.NewVBoxLayout())
	refreshNames := func() {
		a.mu.Lock()
		cmds := make([]CustomCommand, len(a.cfg.CustomCommands))
		copy(cmds, a.cfg.CustomCommands)
		a.mu.Unlock()
		ns := make([]string, len(cmds))
		for i, c := range cmds {
			ns[i] = c.Name
		}
		_ = listBox.SetModel(ns)
	}
	addBtn, _ := walk.NewPushButton(btnCol)
	addBtn.SetText("添加")
	editBtn, _ := walk.NewPushButton(btnCol)
	editBtn.SetText("编辑")
	delBtn, _ := walk.NewPushButton(btnCol)
	delBtn.SetText("删除")
	runBtn, _ := walk.NewPushButton(btnCol)
	runBtn.SetText("立即运行")
	refreshNames()

	addBtn.Clicked().Attach(func() {
		c, ok := a.runCustomCommandDialog(CustomCommand{})
		if !ok {
			return
		}
		a.mu.Lock()
		a.cfg.CustomCommands = append(a.cfg.CustomCommands, c)
		a.mu.Unlock()
		_ = a.saveConfig()
		refreshNames()
	})
	editBtn.Clicked().Attach(func() {
		i := listBox.CurrentIndex()
		a.mu.Lock()
		if i < 0 || i >= len(a.cfg.CustomCommands) {
			a.mu.Unlock()
			_ = walk.MsgBox(a.mw, "提示", "请先选中一条命令", walk.MsgBoxIconInformation)
			return
		}
		cur := a.cfg.CustomCommands[i]
		a.mu.Unlock()
		c, ok := a.runCustomCommandDialog(cur)
		if !ok {
			return
		}
		a.mu.Lock()
		a.cfg.CustomCommands[i] = c
		a.mu.Unlock()
		_ = a.saveConfig()
		refreshNames()
	})
	delBtn.Clicked().Attach(func() {
		i := listBox.CurrentIndex()
		a.mu.Lock()
		if i < 0 || i >= len(a.cfg.CustomCommands) {
			a.mu.Unlock()
			_ = walk.MsgBox(a.mw, "提示", "请先选中一条命令", walk.MsgBoxIconInformation)
			return
		}
		a.cfg.CustomCommands = append(a.cfg.CustomCommands[:i], a.cfg.CustomCommands[i+1:]...)
		a.mu.Unlock()
		_ = a.saveConfig()
		refreshNames()
	})
	runBtn.Clicked().Attach(func() {
		i := listBox.CurrentIndex()
		a.mu.Lock()
		if i < 0 || i >= len(a.cfg.CustomCommands) {
			a.mu.Unlock()
			_ = walk.MsgBox(a.mw, "提示", "请先选中一条命令", walk.MsgBoxIconInformation)
			return
		}
		c := a.cfg.CustomCommands[i]
		a.mu.Unlock()
		a.launchCustom(c)
	})

	// ===== 命令行调用说明（可在隐藏设置中隐藏）=====
	if !a.cfg.Link.HideCmdHelp {
		t3 := boldLabel(page)
		t3.SetText("命令行调用（第三方程序/脚本）")
		help, _ := walk.NewTextEdit(page)
		help.SetReadOnly(true)
		help.SetMinMaxSize(walk.Size{Width: 0, Height: 180}, walk.Size{Width: 0, Height: 500})
		_ = help.SetText(
			"在本程序所在目录打开命令行（或写全路径）：\r\n"+
				"  海信一体机助手.exe --list-actions\r\n"+
				"  海信一体机助手.exe --action screenoff\r\n"+
				"  海信一体机助手.exe --action poweroff --port COM1\r\n"+
				"  海信一体机助手.exe --frame \"DD FF 00 07 C1 FF 00 12 01 00 2A BB CC\"\r\n"+
				"  海信一体机助手.exe --run \"自定义命令名\"\r\n"+
				"参数：--action 动作键；--frame 自定义 HEX；--port 指定串口（缺省取锁定端口）；--run 外部程序。")
	}
}

// runCustomCommandDialog 自定义命令编辑对话框
func (a *App) runCustomCommandDialog(cur CustomCommand) (CustomCommand, bool) {
	dlg, err := walk.NewDialog(a.mw)
	if err != nil {
		return cur, false
	}
	dlg.SetTitle("自定义命令")
	dlg.SetLayout(walk.NewVBoxLayout())
	nameRow, _ := walk.NewComposite(dlg)
	nameRow.SetLayout(walk.NewHBoxLayout())
	nl, _ := walk.NewLabel(nameRow)
	nl.SetText("名称：")
	nameEdit, _ := walk.NewLineEdit(nameRow)
	nameEdit.SetText(cur.Name)

	pathRow, _ := walk.NewComposite(dlg)
	pathRow.SetLayout(walk.NewHBoxLayout())
	pl, _ := walk.NewLabel(pathRow)
	pl.SetText("程序路径：")
	pathEdit, _ := walk.NewLineEdit(pathRow)
	pathEdit.SetText(cur.Path)
	browse, _ := walk.NewPushButton(pathRow)
	browse.SetText("浏览…")
	browse.Clicked().Attach(func() {
		fd := walk.FileDialog{Title: "选择程序", Filter: "程序 (*.exe;*.bat;*.cmd)|*.exe;*.bat;*.cmd|所有文件 (*.*)|*.*"}
		if ok, _ := fd.ShowOpen(a.mw); ok {
			_ = pathEdit.SetText(fd.FilePath)
		}
	})

	argsRow, _ := walk.NewComposite(dlg)
	argsRow.SetLayout(walk.NewHBoxLayout())
	al, _ := walk.NewLabel(argsRow)
	al.SetText("参数：")
	argsEdit, _ := walk.NewLineEdit(argsRow)
	argsEdit.SetText(cur.Args)

	brow, _ := walk.NewComposite(dlg)
	brow.SetLayout(walk.NewHBoxLayout())
	okBtn, _ := walk.NewPushButton(brow)
	okBtn.SetText("确定")
	cancelBtn, _ := walk.NewPushButton(brow)
	cancelBtn.SetText("取消")
	dlg.SetDefaultButton(okBtn)
	dlg.SetCancelButton(cancelBtn)
	result := cur
	okBtn.Clicked().Attach(func() {
		name := strings.TrimSpace(nameEdit.Text())
		path := strings.TrimSpace(pathEdit.Text())
		if name == "" || path == "" {
			_ = walk.MsgBox(dlg, "提示", "名称与程序路径不能为空", walk.MsgBoxIconWarning)
			return
		}
		result = CustomCommand{Name: name, Path: path, Args: strings.TrimSpace(argsEdit.Text())}
		dlg.Accept()
	})
	cancelBtn.Clicked().Attach(dlg.Cancel)
	if dlg.Run() != walk.DlgCmdOK {
		return cur, false
	}
	return result, true
}

// launchCustom 启动外部程序（不阻塞界面）
func (a *App) launchCustom(c CustomCommand) {
	go func() {
		cmd := newOSCmd(c.Path, strings.Fields(c.Args))
		if err := cmd.Start(); err != nil {
			a.logCtrl("自定义命令[%s]启动失败: %v", c.Name, err)
			return
		}
		a.logCtrl("自定义命令[%s]已启动", c.Name)
	}()
}

// launchCustomByName 按名称启动自定义命令（读当前配置）
func (a *App) launchCustomByName(name string) bool {
	a.mu.Lock()
	var found *CustomCommand
	for i := range a.cfg.CustomCommands {
		if a.cfg.CustomCommands[i].Name == name {
			c := a.cfg.CustomCommands[i]
			found = &c
			break
		}
	}
	a.mu.Unlock()
	if found == nil {
		a.logCtrl("自定义命令[%s]不存在", name)
		return false
	}
	a.launchCustom(*found)
	return true
}

// addCustomMenu 向菜单添加“自定义命令”子菜单；hidden=true 时不添加（托盘隐藏选项）
func (a *App) addCustomMenu(parent *walk.Menu, hidden bool) {
	if hidden {
		return
	}
	a.mu.Lock()
	cmds := make([]CustomCommand, len(a.cfg.CustomCommands))
	copy(cmds, a.cfg.CustomCommands)
	a.mu.Unlock()
	if len(cmds) == 0 {
		return
	}
	sub, _ := walk.NewMenu()
	sa, _ := parent.Actions().AddMenu(sub)
	_ = sa.SetText("自定义命令")
	for _, c := range cmds {
		cc := c
		ac := walk.NewAction()
		_ = ac.SetText(cc.Name)
		ac.Triggered().Attach(func() { a.launchCustom(cc) })
		_ = sub.Actions().Add(ac)
	}
}

func atoiSafe(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 9
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
