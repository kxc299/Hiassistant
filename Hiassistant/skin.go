// 皮肤与字体：窗口背景（纯色/图片）、标题栏、可选标签文字色、字号、透明度、全窗口皮肤
// 说明：walk 基于 Win32 原生控件，按钮/复选框/下拉框文字由系统绘制且无法改色；
// 为避免深色皮肤下“按钮文字消失”，本实现只改窗口/容器背景与标签文字，
// 不递归改原生输入控件文字色（上一版曾因此导致菜单与文字消失、字体粗糙）。
package main

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
)

type skinDef struct {
	id, name  string
	bgColor   walk.Color
	textColor walk.Color
	darkTitle bool
}

func skinList() []skinDef {
	return []skinDef{
		{"classic", "经典（系统默认）", walk.RGB(240, 240, 240), walk.RGB(0, 0, 0), false},
		{"custom", "自定义组合（用下方颜色/图片）", walk.RGB(240, 240, 240), walk.RGB(0, 0, 0), false},
		{"blue", "商务蓝", walk.RGB(235, 241, 251), walk.RGB(28, 34, 46), false},
		{"dark", "深色（Dark）", walk.RGB(42, 42, 48), walk.RGB(235, 235, 235), true},
		{"green", "护眼绿", walk.RGB(232, 243, 236), walk.RGB(24, 42, 33), false},
		{"warm", "暖沙色", walk.RGB(245, 239, 229), walk.RGB(48, 38, 26), false},
	}
}

func skinByID(id string) skinDef {
	for _, s := range skinList() {
		if s.id == id {
			return s
		}
	}
	return skinList()[0]
}

func colorKey(c walk.Color) string { return strconv.FormatUint(uint64(c), 16) }

func parseHexColor(s string) (walk.Color, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	if len(s) != 6 {
		return 0, false
	}
	r, e1 := strconv.ParseUint(s[0:2], 16, 8)
	g, e2 := strconv.ParseUint(s[2:4], 16, 8)
	b, e3 := strconv.ParseUint(s[4:6], 16, 8)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, false
	}
	return walk.RGB(byte(r), byte(g), byte(b)), true
}

func colorToHex(c walk.Color) string {
	return fmt.Sprintf("#%02X%02X%02X", byte(c&0xFF), byte((c>>8)&0xFF), byte((c>>16)&0xFF))
}

// chooseColorDialog 调用系统颜色对话框（comdlg32 ChooseColorW），返回用户所选颜色
// 注意：64 位下 CHOOSECOLOR 结构体大小为 60（0x3C），用 unsafe.Sizeof 计算避免读越界。
func (a *App) chooseColorDialog(parentHWND uintptr, cur walk.Color) (walk.Color, bool) {
	cust := [16]uint32{}
	cc := struct {
		LStructSize    uint32
		HwndOwner      uintptr
		HInstance      uintptr
		RgbResult      uint32
		LpCustColors   *[16]uint32
		Flags          uint32
		LCustData      uintptr
		LpfnHook       uintptr
		LpTemplateName *uint16
	}{
		HwndOwner:    parentHWND,
		RgbResult:    uint32(cur),
		LpCustColors: &cust,
		Flags:        0x00000002, // CC_RGBINIT
	}
	cc.LStructSize = uint32(unsafe.Sizeof(cc))
	r, _, _ := syscall.NewLazyDLL("comdlg32.dll").NewProc("ChooseColorW").Call(
		uintptr(unsafe.Pointer(&cc)))
	if r == 0 {
		return cur, false
	}
	return walk.Color(cc.RgbResult), true
}

// applySkin 应用皮肤：背景（自定义图片 > 自定义色 > 皮肤色）、标题栏、字号、标签文字色、透明度、全窗口皮肤
func (a *App) applySkin(id string, fontSize int) {
	bg := skinByID(id).bgColor
	txt := skinByID(id).textColor
	a.mu.Lock()
	img := a.cfg.Link.SkinImage
	customBg := a.cfg.Link.SkinColorBg
	customTxt := a.cfg.Link.SkinColorText
	fullWin := a.cfg.Link.FullWindowSkin
	opacity := a.cfg.Link.Opacity
	a.mu.Unlock()
	if c, ok := parseHexColor(customBg); ok {
		bg = c
	}
	if c, ok := parseHexColor(customTxt); ok {
		txt = c
	}
	if opacity <= 0 || opacity > 100 {
		opacity = 100
	}

	// 背景刷：优先自定义图片（缓存，避免每次重载大图卡顿）
	var br walk.Brush = a.brush(bg)
	useImage := false
	if img != "" {
		if bb := a.imageBrush(img); bb != nil {
			br = bb
			useImage = true
		}
	}

	// 背景：统一使用不透明背景刷（经典=系统灰 RGB240）。
	// 注意：绝不能用 walk.NullBrush()——空画刷会让窗口 WM_ERASEBKGND 不擦除背景，
	// 导致顶部 Tab 标签条（手动控制/定时计划/联动设置/个性化与工具）绘制异常、
	// 一打开程序就“四个菜单消失”。经典色与自定义组合统一用系统灰刷即可。
	classicClean := skinByID(id).id == "classic" && !useImage && customBg == ""
	a.mw.SetBackground(br)
	// 全窗口皮肤：只铺非经典色的内容容器；经典色不递归（避免污染 SplitButton 父链）
	if fullWin && !classicClean {
		a.applyContainerBackground(a.mw, br)
	}

	// 标题栏深色
	a.setDarkTitle(skinByID(id).darkTitle)

	// 字号：仅当显式选择 ≥10 时应用到标签
	if fontSize >= 10 {
		if font, err := walk.NewFont("Microsoft YaHei UI", fontSize, 0); err == nil {
			a.setFontLabels(a.mw, font)
		}
	}

	// 标签文字色：仅当非“经典+无自定义”时应用
	if !(skinByID(id).id == "classic" && !useImage && customBg == "") {
		a.setTextColorLabels(a.mw, txt)
	}

	// 窗口透明度（WS_EX_LAYERED + SetLayeredWindowAttributes）
	a.setWindowOpacity(opacity)
}

// setWindowOpacity 设置主窗口透明度（alpha=1..255，255 不透明）
func (a *App) setWindowOpacity(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	alpha := byte((pct * 255) / 100)
	if alpha == 0 {
		alpha = 1
	}
	hwnd := a.mw.Handle()
	user32 := syscall.NewLazyDLL("user32.dll")
	gwlExStyle := -20 // GWL_EXSTYLE
	ex, _, _ := user32.NewProc("GetWindowLongW").Call(uintptr(hwnd), uintptr(int32(gwlExStyle)))
	layered := uintptr(0x00080000) // WS_EX_LAYERED
	if ex&layered == 0 {
		user32.NewProc("SetWindowLongW").Call(uintptr(hwnd), uintptr(int32(gwlExStyle)), ex|layered)
	}
	// LWA_ALPHA = 0x2
	user32.NewProc("SetLayeredWindowAttributes").Call(uintptr(hwnd), 0, uintptr(alpha), 0x2)
}

// imageBrush 按路径缓存位图背景刷（缩放限制，避免大图平铺卡顿）
func (a *App) imageBrush(path string) walk.Brush {
	if a.skinImages == nil {
		a.skinImages = map[string]*walk.BitmapBrush{}
	}
	if bb, ok := a.skinImages[path]; ok {
		return bb
	}
	bmp, err := walk.NewBitmapFromFileForDPI(path, 96)
	if err != nil {
		a.logCtrl("加载背景图片失败: %v", err)
		return nil
	}
	// 限制超大图：超过 1920x1080 缩放到该范围，减少绘制开销
	s := bmp.Size()
	if s.Width > 1920 || s.Height > 1080 {
		nw, nh := s.Width, s.Height
		if nw > 1920 {
			nh = int(float64(nh) * 1920 / float64(nw))
			nw = 1920
		}
		if nh > 1080 {
			nw = int(float64(nw) * 1080 / float64(nh))
			nh = 1080
		}
		if nb, err2 := walk.NewBitmap(walk.Size{Width: nw, Height: nh}); err2 == nil {
			if cb, cerr := walk.NewCanvasFromImage(nb); cerr == nil {
				_ = cb.DrawImageStretchedPixels(bmp, walk.Rectangle{Width: nw, Height: nh})
				cb.Dispose()
				bmp = nb
			}
		}
	}
	bb, err := walk.NewBitmapBrush(bmp)
	if err != nil {
		return nil
	}
	a.skinImages[path] = bb
	return bb
}

// isManualPage 是否手动控制页（该页含下拉菜单/手动按钮，始终保持系统渲染，
// 避免皮肤背景/文字色干扰 SplitButton 菜单与手动控件）
func (a *App) isManualPage(w walk.Window) bool {
	if tp, ok := w.(*walk.TabPage); ok {
		return tp.Title() == "手动控制"
	}
	return false
}

// applyContainerBackground 递归给内容容器设置背景刷（全窗口皮肤）。
// 注意：不给 TabWidget / TabPage 顶层框架设背景——它们是所有页的父，一旦设刷，
// SplitButton 等原生控件会经 backgroundEffective() 向上回溯到该刷而被污染，
// 导致下拉菜单/按钮在经典色下渲染异常。只铺各页内部的 Composite 内容区。
// 手动控制页整体跳过。
func (a *App) applyContainerBackground(w walk.Window, br walk.Brush) {
	if a.isManualPage(w) {
		return
	}
	if c, ok := w.(walk.Container); ok {
		switch w.(type) {
		case *walk.TabWidget, *walk.TabPage:
			// 顶层框架不直接设背景，仅递归其内容
		default:
			c.SetBackground(br)
		}
		children := c.Children()
		for i := 0; i < children.Len(); i++ {
			a.applyContainerBackground(children.At(i), br)
		}
	}
}

// setFontLabels 仅对 Label 设置字号（跳过手动页）
func (a *App) setFontLabels(w walk.Window, font *walk.Font) {
	if a.isManualPage(w) {
		return
	}
	if l, ok := w.(*walk.Label); ok {
		l.SetFont(font)
		return
	}
	if c, ok := w.(walk.Container); ok {
		children := c.Children()
		for i := 0; i < children.Len(); i++ {
			a.setFontLabels(children.At(i), font)
		}
	}
}

// setTextColorLabels 对文本控件设置文字色（Label/LineEdit/TextEdit/NumberEdit；
// 深色皮肤下若不改，输入框/日志框会被父容器深色背景刷填充成黑字黑底而看不见。
// 按钮/复选框/下拉框等系统自绘控件不改色；跳过手动页。）
func (a *App) setTextColorLabels(w walk.Window, color walk.Color) {
	if a.isManualPage(w) {
		return
	}
	switch t := w.(type) {
	case *walk.Label:
		t.SetTextColor(color)
	case *walk.LineEdit:
		t.SetTextColor(color)
	case *walk.TextEdit:
		t.SetTextColor(color)
	case *walk.NumberEdit:
		t.SetTextColor(color)
	}
	if c, ok := w.(walk.Container); ok {
		children := c.Children()
		for i := 0; i < children.Len(); i++ {
			a.setTextColorLabels(children.At(i), color)
		}
	}
}

// newTabPageLayout 返回带非零边距的 VBoxLayout。
// 根因修复：walk 的 TabWidget 在主窗口设置了皮肤背景刷后，会走自绘标签分支
// （tabwidget.go:437），而该分支仅当 TabPage 布局边距非零时才会调用
// DrawThemeTextEx 绘制标签文字；若边距为零（NewVBoxLayout 默认），整段自绘被跳过，
// 表现为顶部四个标签条（手动控制/定时计划/联动设置/个性化与工具）消失。
// 这里给每个 TabPage 布局一个 9px 边距，使标签文字正常重绘。
func newTabPageLayout() *walk.BoxLayout {
	l := walk.NewVBoxLayout()
	_ = l.SetMargins(walk.Margins{HNear: 9, VNear: 9, HFar: 9, VFar: 9})
	return l
}

// brush 取/建纯色画刷（按颜色缓存）
func (a *App) brush(c walk.Color) *walk.SolidColorBrush {
	if a.skinBrushes == nil {
		a.skinBrushes = map[string]*walk.SolidColorBrush{}
	}
	key := colorKey(c)
	if b, ok := a.skinBrushes[key]; ok {
		return b
	}
	b, err := walk.NewSolidColorBrush(c)
	if err != nil {
		return nil
	}
	a.skinBrushes[key] = b
	return b
}

// setDarkTitle 深色标题栏
func (a *App) setDarkTitle(dark bool) {
	dwm := syscall.NewLazyDLL("dwmapi.dll")
	proc := dwm.NewProc("DwmSetWindowAttribute")
	var val int32
	if dark {
		val = 1
	}
	hwnd := a.mw.Handle()
	for _, attr := range []uint32{20, 19} {
		r, _, _ := proc.Call(uintptr(hwnd), uintptr(attr),
			uintptr(unsafe.Pointer(&val)), unsafe.Sizeof(val))
		if r == 0 {
			break
		}
	}
}
