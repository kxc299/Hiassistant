// 海信教育一体机 助手（GUI 合并版）
// 标签页 1：手动控制（原 1.ps1 控制脚本，含“仅息屏”）
// 标签页 2：定时计划（原 HisenseScheduler 调度器，含 poweroff/sleeppc/screenoff）
// 另含“开机自启”开关；串口直发 9600 8N1。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// newLogBox 带垂直滚动条的只读日志框
func newLogBox(parent walk.Container) *walk.TextEdit {
	t, _ := walk.NewTextEditWithStyle(parent, win.WS_VSCROLL|win.ES_AUTOVSCROLL)
	t.SetReadOnly(true)
	return t
}

// scrollLog 把日志框滚动到最底部（在 UI 线程调用）
func scrollLog(t *walk.TextEdit) {
	if t == nil {
		return
	}
	txt := t.Text()
	t.SetTextSelection(len(txt), len(txt))
	t.ScrollToCaret()
}

// ==================== 命令帧 ====================

const (
	FrameScreenOff = "DD FF 00 07 C1 FF 00 12 01 01 2B BB CC"
	FrameScreenOn  = "DD FF 00 07 C1 FF 00 12 01 00 2A BB CC"
	FramePowerOff  = "DD FF 01 04 A1 01 00 00 BB CC"
	FramePowerOn   = "DD FF 01 04 A1 00 00 00 BB CC"
	FrameReboot    = "DD FF 00 06 C1 1E 00 00 01 D8 BB CC"
	FrameStatus    = "DD FF 00 06 C1 32 00 00 01 F4 BB CC"
	FrameIsOpen    = "DD FF 01 04 A1 10 00 00 BB CC"
	FrameVersion   = "DD FF 00 06 C1 1B 00 00 01 DD BB CC"
	FrameGetID     = "DD FF 00 06 C1 FF 00 0D 01 34 BB CC"
	FrameCancelOn  = "DD FF 00 09 C1 30 00 00 01 00 00 00 F9 BB CC"
	FrameCancelOff = "DD FF 00 09 C1 31 00 00 01 00 00 00 F8 BB CC"
	FrameSignalNow = "DD FF 00 06 C1 1A 00 00 01 DC BB CC" // 查询当前信源
	serialBaud     = 9600
)

// 信号源切换（来自 cmds.xml，A1 族）
func frameSignalSource(kind string) string {
	switch kind {
	case "AV1":
		return "DD FF 01 04 A1 03 01 00 BB CC"
	case "YPbPr":
		return "DD FF 01 04 A1 04 02 00 BB CC"
	case "HDMI1":
		return "DD FF 01 04 A1 05 03 00 BB CC"
	case "HDMI2":
		return "DD FF 01 04 A1 05 03 01 BB CC"
	case "OPS":
		return "DD FF 01 04 A1 05 03 02 BB CC"
	case "VGA1":
		return "DD FF 01 04 A1 06 04 00 BB CC"
	}
	return ""
}

// 图像模式（来自 cmds.xml，C1 0F 06 00 01 0X）
func frameScreenMode(mode byte) string {
	return fmt.Sprintf("DD FF 00 07 C1 0F 06 00 01 %02X %02X BB CC", mode, xorByte(0x07, 0xC1, 0x0F, 0x06, 0x00, 0x01, mode))
}

func xorByte(v ...byte) byte {
	var b byte
	for _, x := range v {
		b ^= x
	}
	return b
}

// 校时（C1 1C 日期 / C1 1D 时间），固定 3 字节参数、无 XOR
func frameSetDate(dt time.Time) string {
	return fmt.Sprintf("DD FF 00 09 C1 1C 00 00 01 %02X %02X %02X BB CC", dt.Year()%100, int(dt.Month()), dt.Day())
}
func frameSetTime(dt time.Time) string {
	return fmt.Sprintf("DD FF 00 09 C1 1D 00 00 01 %02X %02X %02X BB CC", dt.Hour(), dt.Minute(), dt.Second())
}

// 每日定时开/关机（C1 30/31，FF 标记每天，参数 HH MM）
func frameDailyPower(on bool, hh, mm int) string {
	sub := "31"
	if on {
		sub = "30"
	}
	return fmt.Sprintf("DD FF 00 09 C1 %s 00 00 01 FF %02X %02X BB CC", sub, hh, mm)
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValueName = "HisenseAssistant"

func xorHex(core string) string {
	b := 0
	for _, t := range strings.Fields(core) {
		if t == "" {
			continue
		}
		v, _ := strconv.ParseUint(t, 16, 8)
		b ^= int(v)
	}
	return fmt.Sprintf("%02X", b)
}

func buildTimer(typ, hh, mm int, dt time.Time) string {
	sub := "14"
	if typ == 2 {
		sub = "15"
	}
	yH := dt.Year() / 256
	yL := dt.Year() % 256
	core := fmt.Sprintf("00 0D C1 FF 00 %s 01 01 %02X %02X %02X %02X %02X %02X",
		sub, hh, mm, yH, yL, int(dt.Month()), dt.Day())
	return "DD FF " + core + " " + xorHex(core) + " BB CC"
}

func buildByteFrame(cmd byte, val int) string {
	return fmt.Sprintf("DD FF 01 04 A1 %02X 00 %02X BB CC", cmd, val)
}

// 设置通电状态（C1 FF 00 09）：val 语义待实测，通常 0=上电待机、1=上电自启
func buildPowerState(val int) string {
	core := fmt.Sprintf("00 07 C1 FF 00 09 01 %02X", val)
	return "DD FF " + core + " " + xorHex(core) + " BB CC"
}

// ==================== 配置模型 ====================

type FallbackSerial struct {
	COM  string `json:"com"`
	Baud int    `json:"baud"`
}
type Engine struct {
	Enabled          bool           `json:"enabled"`
	GatewayPath      string         `json:"gatewayPath"`
	ConfirmCountdown bool           `json:"confirmCountdown"`
	ManualPort       string         `json:"manualPort"`
	FallbackSerial   FallbackSerial `json:"fallbackSerial"`
}
type Plan struct {
	ID       int    `json:"id"`
	Time     string `json:"time"`
	Action   string `json:"action"`
	Enabled  bool   `json:"enabled"`
	LastDate string `json:"lastDate,omitempty"`
}
type HotkeyBinding struct {
	Action  string `json:"action"` // sleep / screenoff / screenon / status / hide / show
	Mod     uint32 `json:"mod"`    // 1=Alt 2=Ctrl 4=Shift
	VK      uint32 `json:"vk"`     // 虚拟键码
	Enabled bool   `json:"enabled"`
}
// CustomCommand 外部程序（自定义命令），可由菜单/托盘/命令行调用
type CustomCommand struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Args string `json:"args"`
}
type Link struct {
	// 全局快捷键
	HotkeysEnabled bool             `json:"hotkeysEnabled"`
	Hotkeys        []HotkeyBinding  `json:"hotkeys"`
	// 按下 PC 休眠键（VK_SLEEP）自动息屏
	SleepKeyScreenOff bool `json:"sleepKeyScreenOff"`
	// 系统全局休眠/睡眠事件（开始菜单睡眠、合盖、电源键等）自动息屏
	SleepEventScreenOff bool `json:"sleepEventScreenOff"`
	// 拦截休眠键：true=仅息屏不睡眠，false=息屏后继续系统休眠
	SleepKeyBlockSleep bool `json:"sleepKeyBlockSleep"`
	// 息屏守护：定时查询屏幕状态，已息屏但 PC 未休眠则自动休眠
	GuardSleep bool `json:"guardSleepAfterScreenOff"`
	// 守护检测间隔（秒）
	GuardWaitSec int `json:"guardWaitSec"`
	// 应用拦截休眠时不休眠（如播放音乐/视频，powercfg /requests 检测）
	GuardAppBlock bool `json:"guardAppBlock"`
	// 休眠时阻止键鼠唤醒（睡眠期间临时禁用键鼠唤醒，恢复后自动还原）
	WakeBlock bool `json:"wakeBlock"`
	// Windows 关机联动：检测到系统关机时自动关闭整机。
	// 注意：注销(Logoff)会被排除；关机与重启在 Windows 消息层无法可靠区分，
	// 均属于"系统结束会话"，故都会触发。true=启用联动
	WinShutdownLink bool `json:"winShutdownLink"`
	// 皮肤 ID
	Skin string `json:"skin"`
	// 自定义皮肤背景图片路径
	SkinImage string `json:"skinImage"`
	// 自定义背景色 / 文字色（#RRGGBB 或空）
	SkinColorBg   string `json:"skinColorBg"`
	SkinColorText string `json:"skinColorText"`
	// 界面字体大小
	FontSize int `json:"fontSize"`
	// 隐藏选项
	HideCmdHelp       bool `json:"hideCmdHelp"`      // 隐藏个性化页的命令行说明
	HideManualTimer   bool `json:"hideManualTimer"`  // 隐藏手动菜单中的定时写入项
	HideTrayCustomCmd bool `json:"hideTrayCustomCmd"` // 隐藏托盘中的自定义命令子菜单
	HideWakeBlock     bool `json:"hideWakeBlock"`    // 隐藏联动页的“阻止键鼠唤醒”选项
	// 窗口透明度（百分比 0-100，100=不透明）
	Opacity int `json:"opacity"`
	// 全窗口皮肤：开启时递归给所有容器应用皮肤背景色（整窗同色）
	FullWindowSkin bool `json:"fullWindowSkin"`
	// 启动/自启动时最小化到托盘
	StartToTray bool `json:"startToTray"`
	// 完全后台：不显示窗口和托盘（用 Ctrl+Alt+Shift+M 呼出窗口）
	Background bool `json:"background"`
}
type Config struct {
	Engine         Engine          `json:"engine"`
	Link           Link            `json:"link"`
	Schedule       []Plan          `json:"schedule"`
	CustomCommands []CustomCommand `json:"customCommands"`
}

// 自定义命令名缓存（供无接收者的标签/索引辅助函数使用，避免各处重复读锁）
var customNamesCache []string
var customNamesMu sync.Mutex

func cfgCustomNames() []string {
	customNamesMu.Lock()
	defer customNamesMu.Unlock()
	out := make([]string, len(customNamesCache))
	copy(out, customNamesCache)
	return out
}
func setCustomNames(names []string) {
	customNamesMu.Lock()
	customNamesCache = append([]string(nil), names...)
	customNamesMu.Unlock()
}

// 定时动作注册表（覆盖所有可调用功能）；cat 用于分类筛选
type schedAction struct {
	key, label, cat string
}

// 分类标识
const (
	catPower = "power" // 电源
	catSleep = "sleep" // 息屏休眠
	catQuery = "query" // 状态查询
	catSrc   = "src"   // 信源
	catMode  = "mode"  // 图像模式
	catClock = "clock" // 校时
	catCmd   = "cmd"   // 自定义命令（外部程序）
)

var schedActions = []schedAction{
	{"poweroff", "整机关机", catPower},
	{"poweron", "开机", catPower},
	{"reboot", "整机重启", catPower},
	{"sleeppc", "息屏+休眠PC", catSleep},
	{"screenoff", "仅息屏", catSleep},
	{"screenon", "亮屏", catSleep},
	{"status", "查询屏幕状态", catQuery},
	{"isopen", "查询是否开机", catQuery},
	{"version", "查询软件版本", catQuery},
	{"getid", "查询整机ID", catQuery},
	{"signalnow", "查询当前信源", catQuery},
	{"src_ops", "信源→OPS", catSrc},
	{"src_hdmi1", "信源→HDMI1", catSrc},
	{"src_hdmi2", "信源→HDMI2", catSrc},
	{"src_vga1", "信源→VGA1", catSrc},
	{"src_av1", "信源→AV1", catSrc},
	{"src_ypbpr", "信源→YPbPr", catSrc},
	{"mode_standard", "图像模式→标准", catMode},
	{"mode_sport", "图像模式→运动", catMode},
	{"mode_record", "图像模式→录音", catMode},
	{"mode_movie", "图像模式→电影", catMode},
	{"mode_game", "图像模式→游戏", catMode},
	{"syncclock", "同步本机时间到整机", catClock},
}

// 分类筛选（显示顺序与中文标签）
var schedCategories = []struct{ id, label string }{
	{"", "全部"},
	{catPower, "电源"},
	{catSleep, "息屏/休眠"},
	{catQuery, "状态查询"},
	{catSrc, "信号源"},
	{catMode, "图像模式"},
	{catClock, "校时"},
	{catCmd, "自定义命令"},
}

// 是否为自定义命令动作（Plan.Action / 热键 Action 前缀 cmd:）
func isCustomAction(key string) bool { return strings.HasPrefix(key, "cmd:") }
func customActionName(key string) string { return strings.TrimPrefix(key, "cmd:") }

func categoryOf(key string) string {
	if isCustomAction(key) {
		return catCmd
	}
	for _, a := range schedActions {
		if a.key == key {
			return a.cat
		}
	}
	return ""
}

// 定时查询类动作（需读取回执）
var schedQueryActions = map[string]bool{
	"status": true, "isopen": true, "version": true, "getid": true, "signalnow": true,
}

func schedActionLabel(key string) string {
	if isCustomAction(key) {
		return "自定义命令:" + customActionName(key) + " (" + key + ")"
	}
	for _, a := range schedActions {
		if a.key == key {
			return a.label + " (" + a.key + ")"
		}
	}
	return key
}
func schedActionLabels() []string {
	return schedActionLabelsByCat("")
}
func schedActionLabelsByCat(cat string) []string {
	var out []string
	// 自定义命令分类
	if cat == "" || cat == catCmd {
		names := cfgCustomNames()
		for _, n := range names {
			key := "cmd:" + n
			out = append(out, "自定义命令:"+n+" ("+key+")")
		}
	}
	for _, a := range schedActions {
		if cat == catCmd {
			break
		}
		if cat == "" || a.cat == cat {
			out = append(out, a.label+" ("+a.key+")")
		}
	}
	return out
}
func schedIndexOf(key string) int {
	return schedIndexOfInCat(key, "")
}
func schedIndexOfInCat(key, cat string) int {
	idx := 0
	if cat == "" || cat == catCmd {
		names := cfgCustomNames()
		for _, n := range names {
			k := "cmd:" + n
			if k == key {
				return idx
			}
			idx++
		}
	}
	for _, a := range schedActions {
		if cat == catCmd {
			break
		}
		if cat != "" && a.cat != cat {
			continue
		}
		if a.key == key {
			return idx
		}
		idx++
	}
	return 0
}
func schedKeyFromLabel(text string) string {
	if i := strings.LastIndex(text, "("); i >= 0 {
		if j := strings.LastIndex(text, ")"); j > i {
			return strings.TrimSpace(text[i+1 : j])
		}
	}
	return strings.TrimSpace(text)
}
func schedActionFrame(act string) (string, bool) {
	switch act {
	case "poweroff":
		return FramePowerOff, true
	case "poweron":
		return FramePowerOn, true
	case "reboot":
		return FrameReboot, true
	case "screenoff", "sleeppc":
		return FrameScreenOff, true
	case "screenon":
		return FrameScreenOn, true
	case "status":
		return FrameStatus, true
	case "isopen":
		return FrameIsOpen, true
	case "version":
		return FrameVersion, true
	case "getid":
		return FrameGetID, true
	case "signalnow":
		return FrameSignalNow, true
	case "src_ops":
		return frameSignalSource("OPS"), true
	case "src_hdmi1":
		return frameSignalSource("HDMI1"), true
	case "src_hdmi2":
		return frameSignalSource("HDMI2"), true
	case "src_vga1":
		return frameSignalSource("VGA1"), true
	case "src_av1":
		return frameSignalSource("AV1"), true
	case "src_ypbpr":
		return frameSignalSource("YPbPr"), true
	case "mode_standard":
		return frameScreenMode(0x07), true
	case "mode_sport":
		return frameScreenMode(0x08), true
	case "mode_record":
		return frameScreenMode(0x09), true
	case "mode_movie":
		return frameScreenMode(0x0A), true
	case "mode_game":
		return frameScreenMode(0x0B), true
	}
	return "", false
}

// 兼容旧调用
func actionLabel(a string) string { return schedActionLabel(a) }
func actionFromLabel(label string) string { return schedKeyFromLabel(label) }

// ==================== 通用串口 ====================

func listPorts() []string {
	var ports []string
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DEVICEMAP\SERIALCOMM`, registry.QUERY_VALUE)
	if err == nil {
		names, _ := k.ReadValueNames(-1)
		for _, n := range names {
			v, _, err := k.GetStringValue(n)
			if err == nil && v != "" {
				ports = append(ports, v)
			}
		}
		k.Close()
	}
	sort.Strings(ports)
	seen := map[string]bool{}
	out := ports[:0]
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func serialOpen(port string) (windows.Handle, error) {
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(`\\.\`+port),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("打开 %s 失败: %w", port, err)
	}
	var dcb windows.DCB
	dcb.DCBlength = uint32(unsafe.Sizeof(dcb))
	if err := windows.GetCommState(h, &dcb); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	dcb.BaudRate = serialBaud
	dcb.ByteSize = 8
	dcb.Parity = 0
	dcb.StopBits = 0
	dcb.Flags = 0x01 | 0x10
	if err := windows.SetCommState(h, &dcb); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	to := windows.CommTimeouts{
		ReadIntervalTimeout: 50, ReadTotalTimeoutMultiplier: 10, ReadTotalTimeoutConstant: 800,
		WriteTotalTimeoutMultiplier: 10, WriteTotalTimeoutConstant: 1000,
	}
	if err := windows.SetCommTimeouts(h, &to); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	_ = windows.PurgeComm(h, windows.PURGE_RXCLEAR|windows.PURGE_TXCLEAR)
	return h, nil
}

func hexToBytes(s string) ([]byte, error) {
	c := strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "").Replace(s)
	if len(c)%2 != 0 {
		return nil, fmt.Errorf("hex 长度非法")
	}
	b := make([]byte, len(c)/2)
	for i := range b {
		v, err := strconv.ParseUint(c[i*2:i*2+2], 16, 8)
		if err != nil {
			return nil, err
		}
		b[i] = byte(v)
	}
	return b, nil
}

func toHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	out := make([]string, len(b))
	for i, v := range b {
		out[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(out, " ")
}

// serialMu 串行化所有串口操作，避免守护轮询与手动/定时发送抢占同一串口（Access is denied）
var serialMu sync.Mutex

// serialOpenRetry 打开串口；遇到被占用（Access is denied）时短暂重试
func serialOpenRetry(port string) (windows.Handle, error) {
	var lastErr error
	for i := 0; i < 5; i++ {
		h, err := serialOpen(port)
		if err == nil {
			return h, nil
		}
		lastErr = err
		if i < 4 {
			time.Sleep(250 * time.Millisecond)
		}
	}
	return 0, lastErr
}

// serialSend 直发并读取回执（手动控制用）
func serialSend(port, hex string) (string, error) {
	data, err := hexToBytes(hex)
	if err != nil {
		return "", err
	}
	serialMu.Lock()
	defer serialMu.Unlock()
	h, err := serialOpenRetry(port)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var written uint32
	if err := windows.WriteFile(h, data, &written, nil); err != nil {
		return "", err
	}
	time.Sleep(700 * time.Millisecond)
	var resp []byte
	buf := make([]byte, 256)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		var n uint32
		if err := windows.ReadFile(h, buf, &n, nil); err == nil && n > 0 {
			resp = append(resp, buf[:n]...)
			if len(resp) >= 256 {
				break
			}
			time.Sleep(120 * time.Millisecond)
			var n2 uint32
			if windows.ReadFile(h, buf, &n2, nil) == nil && n2 > 0 {
				resp = append(resp, buf[:n2]...)
			}
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	return toHex(resp), nil
}

// serialWrite 只写帧（定时调度/守护用）
func serialWrite(port, hex string) error {
	data, err := hexToBytes(hex)
	if err != nil {
		return err
	}
	serialMu.Lock()
	defer serialMu.Unlock()
	h, err := serialOpenRetry(port)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var written uint32
	return windows.WriteFile(h, data, &written, nil)
}

// sleepPC 让 PC 进入睡眠（优先用系统 API，无黑窗；API 失败再回退隐藏窗口的 shutdown）
func sleepPC() {
	pp := windows.NewLazySystemDLL("powrprof.dll").NewProc("SetSuspendState")
	// bHibernate=0 睡眠，bForce=1 强制进入（忽略应用拒绝），bDisableWakeEvent=0
	if r, _, _ := pp.Call(0, 1, 0); r == 0 {
		// 睡眠失败：尝试休眠（无窗口）
		if r2, _, _ := pp.Call(1, 1, 0); r2 == 0 {
			cmd := exec.Command("shutdown.exe", "/h", "/f")
			cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
			_ = cmd.Start()
		}
	}
}

// ==================== 开机自启 ====================

func isAutoStart() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false, nil
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValueName)
	if err != nil {
		return false, nil
	}
	return v != "", nil
}

func setAutoStart(enable bool, exePath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if enable {
		return k.SetStringValue(runValueName, `"`+exePath+`" --minimized`)
	}
	return k.DeleteValue(runValueName)
}

// ==================== 应用 ====================

type App struct {
	dir, cfgPath, schedLogPath, ctrlLogPath, crashPath string
	mu                                                  sync.Mutex
	cfg                                                 Config

	mw *walk.MainWindow

	// 手动控制
	portCombo *walk.ComboBox
	mlog      *walk.TextEdit
	mlines    []string
	volNE     *walk.NumberEdit
	briNE     *walk.NumberEdit
	timerType *walk.ComboBox
	timerTime *walk.LineEdit
	rawEdit   *walk.LineEdit

	// 定时计划
	table     *walk.TableView
	smodel    *PlanModel
	allPlans  []*Plan
	slog      *walk.TextEdit
	slines    []string
	engineCk  *walk.CheckBox
	planFilter string
	confirmCk *walk.CheckBox
	autoCk    *walk.CheckBox
	startBtn  *walk.PushButton
	stopBtn   *walk.PushButton
	statusBar *walk.StatusBarItem

	schedCancel chan struct{}
	schedMu     sync.Mutex
	running     bool

	// 联动（热键/休眠键/守护）
	hub      *hotkeyHub
	guardMu  sync.Mutex
	guarding bool
	tray     *walk.NotifyIcon
	hkCk     *walk.CheckBox
	sleepCk  *walk.CheckBox
	sleepEvtCk *walk.CheckBox
	blockRb  *walk.RadioButton
	passRb   *walk.RadioButton
	guardCk  *walk.CheckBox
	guardNE  *walk.NumberEdit
	appBlockCk *walk.CheckBox
	wakeBlockCk *walk.CheckBox
	winShutdownCk *walk.CheckBox
	trayCk   *walk.CheckBox
	bgCk     *walk.CheckBox
	llog     *walk.TextEdit

	// 皮肤资源
	skinBrushes map[string]*walk.SolidColorBrush
	skinImages  map[string]*walk.BitmapBrush
	tabs        *walk.TabWidget
	// 本次睡眠被临时禁用唤醒的设备（恢复后重新启用）
	wakeBlocked []string
}

func appendFile(path, s string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		f.WriteString(s)
		f.Close()
	}
}

func (a *App) logCtrl(format string, args ...interface{}) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	a.mu.Lock()
	a.mlines = append(a.mlines, line)
	if len(a.mlines) > 1000 {
		a.mlines = a.mlines[len(a.mlines)-1000:]
	}
	txt := strings.Join(a.mlines, "\r\n")
	a.mu.Unlock()
	appendFile(a.ctrlLogPath, line+"\n")
	if a.mw != nil {
		a.mw.Synchronize(func() {
			if a.mlog != nil {
				_ = a.mlog.SetText(txt)
				scrollLog(a.mlog)
			}
			if a.llog != nil {
				_ = a.llog.SetText(txt)
				scrollLog(a.llog)
			}
		})
	}
}

func (a *App) logSched(format string, args ...interface{}) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	a.mu.Lock()
	a.slines = append(a.slines, line)
	if len(a.slines) > 2000 {
		a.slines = a.slines[len(a.slines)-2000:]
	}
	txt := strings.Join(a.slines, "\r\n")
	a.mu.Unlock()
	appendFile(a.schedLogPath, line+"\n")
	if a.mw != nil && a.slog != nil {
		a.mw.Synchronize(func() {
			_ = a.slog.SetText(txt)
			scrollLog(a.slog)
		})
	}
}

// ==================== 配置 ====================

func (a *App) loadConfig() error {
	data, err := os.ReadFile(a.cfgPath)
	if err != nil {
		return err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for i := range cfg.Schedule {
		if n, ok := normalizePlanTime(cfg.Schedule[i].Time); ok {
			cfg.Schedule[i].Time = n
		}
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	names := make([]string, len(cfg.CustomCommands))
	for i, c := range cfg.CustomCommands {
		names[i] = c.Name
	}
	setCustomNames(names)
	return nil
}
func (a *App) saveConfig() error {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.cfgPath, data, 0644)
}

// ==================== 手动控制 ====================

func (a *App) ctrlPort() string {
	if a.portCombo == nil {
		return ""
	}
	return strings.TrimSpace(a.portCombo.Text())
}

func (a *App) ctrlRun(name, frame string, interpret func(string)) {
	port := a.ctrlPort()
	if port == "" {
		_ = walk.MsgBox(a.mw, "提示", "请先在“手动控制”页选择/扫描串口", walk.MsgBoxIconInformation)
		return
	}
	a.logCtrl("发送[%s] %s", name, frame)
	go func() {
		resp, err := serialSend(port, frame)
		a.mw.Synchronize(func() {
			if err != nil {
				a.logCtrl("  失败: %v", err)
				return
			}
			if resp == "" {
				a.logCtrl("  无回执（可能已执行但未返回）")
				return
			}
			a.logCtrl("  回执: %s", resp)
			if interpret != nil {
				interpret(resp)
			}
		})
	}()
}

func (a *App) refreshPorts() {
	ports := listPorts()
	_ = a.portCombo.SetModel(ports)
	if len(ports) == 0 {
		a.logCtrl("未发现 COM 口，请插入 USB 转串口后点“刷新端口”")
		return
	}
	a.logCtrl("可用串口: %v", ports)
	// 优先选中已锁定的端口
	a.mu.Lock()
	saved := a.cfg.Engine.ManualPort
	a.mu.Unlock()
	if saved != "" {
		for _, p := range ports {
			if strings.EqualFold(p, saved) {
				a.portCombo.SetText(p)
				return
			}
		}
	}
	a.portCombo.SetCurrentIndex(0)
}

// persistManualPort 保存当前选中的串口（端口锁定）
func (a *App) persistManualPort() {
	port := strings.TrimSpace(a.portCombo.Text())
	if port == "" {
		return
	}
	a.mu.Lock()
	a.cfg.Engine.ManualPort = port
	a.mu.Unlock()
	_ = a.saveConfig()
}

func (a *App) autoDetect() {
	go func() {
		ports := listPorts()
		a.logCtrl("自动探测串口…")
		found := ""
		for _, p := range ports {
			resp, err := serialSend(p, FrameStatus)
			if err == nil && strings.Contains(resp, "AB AB") {
				found = p
				break
			}
		}
		a.mw.Synchronize(func() {
			if found != "" {
				a.portCombo.SetText(found)
				a.logCtrl("  探测到一体机串口: %s", found)
				_ = walk.MsgBox(a.mw, "探测结果", "找到一体机串口: "+found, walk.MsgBoxIconInformation)
			} else {
				a.logCtrl("  未在任何串口收到 AB AB 回执")
				_ = walk.MsgBox(a.mw, "探测结果", "未探测到一体机，请检查接线/串口。", walk.MsgBoxIconWarning)
			}
		})
	}()
}

// writeRTC 按下方输入框的时间写入整机 RTC 定时（typ 1=开机 2=关机）
func (a *App) writeRTC(typ int) {
	hh, mm, ok := parseHHMM(a.timerTime.Text())
	if !ok {
		_ = walk.MsgBox(a.mw, "提示", "时间格式应为 HH:mm，例如 18:00", walk.MsgBoxIconWarning)
		return
	}
	name := "定时开机"
	if typ == 2 {
		name = "定时关机"
	}
	a.ctrlRun(name, buildTimer(typ, hh, mm, time.Now()), nil)
}

type hkRow struct {
	action   string
	en       *walk.CheckBox
	ctrl, al, sh *walk.CheckBox
	key      *walk.ComboBox
}

func (a *App) buildLinkTab(tab *walk.TabPage) {
	tab.SetLayout(newTabPageLayout())
	sv, _ := walk.NewScrollView(tab)
	sv.SetScrollbars(false, true)
	sv.SetLayout(walk.NewVBoxLayout())
	var page walk.Container = sv

	var rows []*hkRow
	// 保存当前联动设置
	saveLink := func() {
		a.mu.Lock()
		a.cfg.Link.HotkeysEnabled = a.hkCk.Checked()
		a.cfg.Link.SleepKeyScreenOff = a.sleepCk.Checked()
		a.cfg.Link.SleepEventScreenOff = a.sleepEvtCk.Checked()
		a.cfg.Link.SleepKeyBlockSleep = a.blockRb.Checked()
		a.cfg.Link.GuardSleep = a.guardCk.Checked()
		a.cfg.Link.GuardWaitSec = int(a.guardNE.Value())
		a.cfg.Link.GuardAppBlock = a.appBlockCk.Checked()
		a.cfg.Link.WakeBlock = a.wakeBlockCk.Checked()
		a.cfg.Link.WinShutdownLink = a.winShutdownCk.Checked()
		a.cfg.Link.StartToTray = a.trayCk.Checked()
		a.cfg.Link.Background = a.bgCk.Checked()
		var binds []HotkeyBinding
		for _, r := range rows {
			var mod uint32
			if r.ctrl.Checked() {
				mod |= modControl
			}
			if r.al.Checked() {
				mod |= modAlt
			}
			if r.sh.Checked() {
				mod |= modShift
			}
			binds = append(binds, HotkeyBinding{
				Action: r.action, Mod: mod,
				VK: vkFromLabel(r.key.Text()), Enabled: r.en.Checked(),
			})
		}
		a.cfg.Link.Hotkeys = binds
		a.mu.Unlock()
		_ = a.saveConfig()
		if a.hub != nil {
			a.hub.requestApply()
		}
	}

	// ===== 全局快捷键（自定义、防冲突） =====
	title1 := boldLabel(page)
	title1.SetText("全局快捷键（可自定义，系统会自动检测并跳过冲突/被占用组合）")
	a.hkCk, _ = walk.NewCheckBox(page)
	a.hkCk.SetText("启用全局快捷键")
	a.hkCk.SetChecked(a.cfg.Link.HotkeysEnabled)

	// 固定的呼出热键提示
	fixed, _ := walk.NewLabel(page)
	fixed.SetText("  固定呼出热键：Ctrl+Alt+Shift+M（后台/托盘模式下用于恢复主窗口，始终有效）")

	// 表头与各行编辑
	keyOptions := hotkeyKeyOptions()
	cfgByAction := map[string]HotkeyBinding{}
	for _, b := range a.cfg.Link.Hotkeys {
		cfgByAction[b.Action] = b
	}
	for _, action := range []string{"sleep", "screenoff", "screenon", "status", "show", "hide"} {
		row, _ := walk.NewComposite(page)
		row.SetLayout(walk.NewHBoxLayout())
		en, _ := walk.NewCheckBox(row)
		en.SetChecked(true)
		nm, _ := walk.NewLabel(row)
		nm.SetText(hotkeyActionName(action))
		nm.SetMinMaxSize(walk.Size{Width: 170, Height: 0}, walk.Size{Width: 200, Height: 0})
		cc, _ := walk.NewCheckBox(row)
		cc.SetText("Ctrl")
		ac, _ := walk.NewCheckBox(row)
		ac.SetText("Alt")
		sc, _ := walk.NewCheckBox(row)
		sc.SetText("Shift")
		kl, _ := walk.NewLabel(row)
		kl.SetText("按键:")
		kc, _ := walk.NewComboBox(row)
		_ = kc.SetModel(keyOptions)
		// 旧配置缺少该动作时使用内置默认
		b, ok := cfgByAction[action]
		if !ok {
			b = builtinHotkey(action)
		}
		en.SetChecked(b.Enabled)
		cc.SetChecked(b.Mod&modControl != 0)
		ac.SetChecked(b.Mod&modAlt != 0)
		sc.SetChecked(b.Mod&modShift != 0)
		idx := indexOfKeyLabel(keyOptions, labelFromVK(b.VK))
		if idx < 0 {
			idx = 0
		}
		_ = kc.SetCurrentIndex(idx)
		hr := &hkRow{action: action, en: en, ctrl: cc, al: ac, sh: sc, key: kc}
		rows = append(rows, hr)
	}
	// 自定义命令快捷键绑定（动作键 cmd:名称）
	a.mu.Lock()
	cmdsForHotkey := make([]CustomCommand, len(a.cfg.CustomCommands))
	copy(cmdsForHotkey, a.cfg.CustomCommands)
	a.mu.Unlock()
	for _, cm := range cmdsForHotkey {
		cmdAction := "cmd:" + cm.Name
		row, _ := walk.NewComposite(page)
		row.SetLayout(walk.NewHBoxLayout())
		en, _ := walk.NewCheckBox(row)
		en.SetChecked(true)
		nm, _ := walk.NewLabel(row)
		nm.SetText("自定义命令:" + cm.Name)
		nm.SetMinMaxSize(walk.Size{Width: 170, Height: 0}, walk.Size{Width: 200, Height: 0})
		cc, _ := walk.NewCheckBox(row)
		cc.SetText("Ctrl")
		ac, _ := walk.NewCheckBox(row)
		ac.SetText("Alt")
		sc, _ := walk.NewCheckBox(row)
		sc.SetText("Shift")
		kl, _ := walk.NewLabel(row)
		kl.SetText("按键:")
		kc, _ := walk.NewComboBox(row)
		_ = kc.SetModel(keyOptions)
		b, ok := cfgByAction[cmdAction]
		if !ok {
			b = HotkeyBinding{Action: cmdAction, Mod: modAlt | modControl, Enabled: true}
		}
		en.SetChecked(b.Enabled)
		cc.SetChecked(b.Mod&modControl != 0)
		ac.SetChecked(b.Mod&modAlt != 0)
		sc.SetChecked(b.Mod&modShift != 0)
		idx := indexOfKeyLabel(keyOptions, labelFromVK(b.VK))
		if idx < 0 {
			idx = 0
		}
		_ = kc.SetCurrentIndex(idx)
		rows = append(rows, &hkRow{action: cmdAction, en: en, ctrl: cc, al: ac, sh: sc, key: kc})
	}
	note1, _ := walk.NewLabel(page)
	note1.SetText("  提示：同一组合若被占用，系统会在日志提示“注册失败”，请更换；同一动作内重复组合会被自动跳过。")

	// ===== 托盘 / 后台 =====
	title0 := boldLabel(page)
	title0.SetText("托盘与后台运行")
	a.trayCk, _ = walk.NewCheckBox(page)
	a.trayCk.SetText("启动/开机自启时最小化到托盘（窗口隐藏，托盘图标可见）")
	a.trayCk.SetChecked(a.cfg.Link.StartToTray)
	a.bgCk, _ = walk.NewCheckBox(page)
	a.bgCk.SetText("完全后台运行（不显示窗口和托盘；用 Ctrl+Alt+Shift+M 呼出）")
	a.bgCk.SetChecked(a.cfg.Link.Background)
	hideBtn, _ := walk.NewPushButton(page)
	hideBtn.SetText("立即隐藏到托盘")
	hideBtn.Clicked().Attach(func() { a.hideToTray(false) })
	note0, _ := walk.NewLabel(page)
	note0.SetText("  开机自启项会以 --minimized 启动；勾选“完全后台”后窗口与托盘均不显示，可用固定呼出热键恢复。")

	// ===== 休眠键联动 =====
	title2 := boldLabel(page)
	title2.SetText("PC 休眠键联动（键盘 Sleep 键）")
	a.sleepCk, _ = walk.NewCheckBox(page)
	a.sleepCk.SetText("检测到按下 PC 休眠键时，自动对整机关闭显示")
	a.sleepCk.SetChecked(a.cfg.Link.SleepKeyScreenOff)
	rbRow, _ := walk.NewComposite(page)
	rbRow.SetLayout(walk.NewHBoxLayout())
	a.blockRb, _ = walk.NewRadioButton(rbRow)
	a.blockRb.SetText("仅息屏，并拦截系统休眠")
	a.passRb, _ = walk.NewRadioButton(rbRow)
	a.passRb.SetText("息屏后继续让 PC 休眠")
	if a.cfg.Link.SleepKeyBlockSleep {
		a.blockRb.SetChecked(true)
	} else {
		a.passRb.SetChecked(true)
	}
	note2, _ := walk.NewLabel(page)
	note2.SetText("  说明：部分无线键盘/一体机没有单独的 Sleep 键；拦截休眠需低级键盘钩子，可能被安全软件提示。")

	// ===== 系统全局休眠事件检测（开始菜单睡眠/合盖/电源键） =====
	a.sleepEvtCk, _ = walk.NewCheckBox(page)
	a.sleepEvtCk.SetText("检测到系统全局休眠（开始菜单“睡眠”、合盖、电源键等）时，自动对整机关闭显示")
	a.sleepEvtCk.SetChecked(a.cfg.Link.SleepEventScreenOff)
	note2b, _ := walk.NewLabel(page)
	note2b.SetText("  说明：通过系统电源广播（WM_POWERBROADCAST）在真正进入睡眠前息屏，不依赖键盘 Sleep 键。")

	// ===== 息屏守护 =====
	title3 := boldLabel(page)
	title3.SetText("息屏守护（定时查询屏幕状态；已息屏但 PC 未休眠则自动休眠）")
	a.guardCk, _ = walk.NewCheckBox(page)
	a.guardCk.SetText("启用息屏守护：定时查询屏幕状态，已息屏但 PC 未休眠则自动休眠")
	a.guardCk.SetChecked(a.cfg.Link.GuardSleep)
	gRow, _ := walk.NewComposite(page)
	gRow.SetLayout(walk.NewHBoxLayout())
	gl, _ := walk.NewLabel(gRow)
	gl.SetText("检测间隔（秒）：")
	a.guardNE, _ = walk.NewNumberEdit(gRow)
	a.guardNE.SetRange(10, 1800)
	a.guardNE.SetDecimals(0)
	wait := a.cfg.Link.GuardWaitSec
	if wait <= 0 {
		wait = 60
	}
	a.guardNE.SetValue(float64(wait))
	a.appBlockCk, _ = walk.NewCheckBox(page)
	a.appBlockCk.SetText("应用拦截休眠时不休眠（检测到播放音乐/视频等占用，powercfg /requests）")
	a.appBlockCk.SetChecked(a.cfg.Link.GuardAppBlock)
	a.wakeBlockCk, _ = walk.NewCheckBox(page)
	a.wakeBlockCk.SetText("休眠时阻止键鼠唤醒计算机（睡眠期间禁用键鼠唤醒，恢复后自动还原；可能需管理员）")
	a.wakeBlockCk.SetChecked(a.cfg.Link.WakeBlock)
	a.mu.Lock()
	hideWB := a.cfg.Link.HideWakeBlock
	a.mu.Unlock()
	if hideWB {
		a.wakeBlockCk.SetVisible(false)
	}
	a.winShutdownCk, _ = walk.NewCheckBox(page)
	a.winShutdownCk.SetText("检测到 Windows 关机/重启时自动关闭整机（注销不联动；关机与重启消息层无法区分，均会触发）")
	a.winShutdownCk.SetChecked(a.cfg.Link.WinShutdownLink)
	note3, _ := walk.NewLabel(page)
	note3.SetText("  说明：守护对“仅息屏”菜单、热键及定时 screenoff 均生效；最近 8 秒有键鼠活动时本轮跳过；\r\n  应用拦截与阻止唤醒选项对程序内所有睡眠动作（一键息屏/定时/守护）统一生效。")

	// 保存按钮
	btnRow, _ := walk.NewComposite(page)
	btnRow.SetLayout(walk.NewHBoxLayout())
	save, _ := walk.NewPushButton(btnRow)
	save.SetText("保存并应用")
	save.Clicked().Attach(func() {
		saveLink()
		_ = walk.MsgBox(a.mw, "提示", "联动设置已保存并应用", walk.MsgBoxIconInformation)
	})
	a.hkCk.Clicked().Attach(saveLink)
	a.trayCk.Clicked().Attach(saveLink)
	a.bgCk.Clicked().Attach(saveLink)
	a.sleepCk.Clicked().Attach(saveLink)
	a.sleepEvtCk.Clicked().Attach(saveLink)
	a.blockRb.Clicked().Attach(saveLink)
	a.passRb.Clicked().Attach(saveLink)
	a.guardCk.Clicked().Attach(saveLink)
	a.appBlockCk.Clicked().Attach(saveLink)
	a.wakeBlockCk.Clicked().Attach(saveLink)
	a.winShutdownCk.Clicked().Attach(saveLink)

	ll, _ := walk.NewLabel(page)
	ll.SetText("联动日志（同时写入 control.log）：")
	a.llog = makeLinkLog(page)
}

// 快捷键可选按键
func hotkeyKeyOptions() []string {
	var opts []string
	for c := 'A'; c <= 'Z'; c++ {
		opts = append(opts, string(c))
	}
	for i := 1; i <= 12; i++ {
		opts = append(opts, "F"+strconv.Itoa(i))
	}
	for c := '0'; c <= '9'; c++ {
		opts = append(opts, string(c))
	}
	return opts
}
func vkFromLabel(s string) uint32 {
	s = strings.TrimSpace(s)
	if len(s) == 1 {
		r := rune(s[0])
		if r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' {
			return uint32(r)
		}
	}
	if strings.HasPrefix(s, "F") {
		if n, err := strconv.Atoi(s[1:]); err == nil && n >= 1 && n <= 12 {
			return uint32(0x6F + n)
		}
	}
	return 0
}
func labelFromVK(vk uint32) string {
	if vk >= 'A' && vk <= 'Z' || vk >= '0' && vk <= '9' {
		return string(rune(vk))
	}
	if vk >= 0x70 && vk <= 0x7B {
		return "F" + strconv.Itoa(int(vk-0x6F))
	}
	return ""
}
func indexOfKeyLabel(opts []string, v string) int {
	for i, o := range opts {
		if o == v {
			return i
		}
	}
	return -1
}


func boldLabel(parent walk.Container) *walk.Label {
	l, _ := walk.NewLabel(parent)
	if f, err := walk.NewFont("Microsoft YaHei", 10, walk.FontBold); err == nil {
		l.SetFont(f)
	}
	return l
}
func makeLinkLog(parent walk.Container) *walk.TextEdit {
	t := newLogBox(parent)
	t.SetMinMaxSize(walk.Size{Width: 0, Height: 160}, walk.Size{Width: 0, Height: 800})
	t.SetText("就绪。")
	return t
}

func (a *App) buildManualTab(tab *walk.TabPage) {
	tab.SetLayout(newTabPageLayout())
	sv, _ := walk.NewScrollView(tab)
	sv.SetScrollbars(false, true)
	sv.SetLayout(walk.NewVBoxLayout())
	var page walk.Container = sv
	portRow, _ := walk.NewComposite(page)
	portRow.SetLayout(walk.NewHBoxLayout())
	lb, _ := walk.NewLabel(portRow)
	lb.SetText("串口：")
	a.portCombo, _ = walk.NewComboBox(portRow)
	a.portCombo.TextChanged().Attach(a.persistManualPort)
	b1, _ := walk.NewPushButton(portRow)
	b1.SetText("刷新端口")
	b1.Clicked().Attach(a.refreshPorts)
	b2, _ := walk.NewPushButton(portRow)
	b2.SetText("自动探测")
	b2.Clicked().Attach(a.autoDetect)

	// ===== 主操作：一键息屏（SplitButton 主点击=息屏+休眠；下拉=更多功能），旁边亮屏 =====
	menuRow, _ := walk.NewComposite(page)
	menuRow.SetLayout(walk.NewHBoxLayout())
	sb, _ := walk.NewSplitButton(menuRow)
	sb.SetText("一键息屏（息屏+休眠）  ▾")
	sb.SetMinMaxSize(walk.Size{Width: 220, Height: 0}, walk.Size{Width: 300, Height: 0})
	sb.Clicked().Attach(func() {
		a.ctrlRun("息屏", FrameScreenOff, nil)
		// 睡眠放到后台，避免在 UI 线程阻塞（旧实现会在界面线程 Sleep+SetSuspendState）
		go func() {
			time.Sleep(700 * time.Millisecond)
			a.managedSleep("一键息屏")
			a.logCtrl("  已触发 PC 睡眠")
		}()
	})
	wakeBtn, _ := walk.NewPushButton(menuRow)
	wakeBtn.SetText("亮屏")
	wakeBtn.Clicked().Attach(func() { a.ctrlRun("亮屏", FrameScreenOn, nil) })

	// 常用动作（供菜单与按钮复用）
	doScreenOff := func() {
		a.ctrlRun("仅息屏", FrameScreenOff, nil)
		a.startScreenOffGuard()
	}
	doPowerOff := func() {
		if walk.MsgBox(a.mw, "确认", "确定立即整机关机？", walk.MsgBoxIconQuestion|walk.MsgBoxYesNo) != walk.DlgCmdYes {
			return
		}
		a.ctrlRun("整机关机", FramePowerOff, nil)
	}
	doReboot := func() {
		if walk.MsgBox(a.mw, "确认", "确定立即整机重启？", walk.MsgBoxIconQuestion|walk.MsgBoxYesNo) != walk.DlgCmdYes {
			return
		}
		a.ctrlRun("重启", FrameReboot, nil)
	}
	doStatus := func() {
		a.ctrlRun("屏幕状态", FrameStatus, func(resp string) {
			switch {
			case strings.Contains(resp, "01 F4"):
				a.logCtrl("  -> 屏幕状态：亮屏")
			case strings.Contains(resp, "00 F5"):
				a.logCtrl("  -> 屏幕状态：息屏")
			default:
				a.logCtrl("  -> 屏幕状态：未能判定")
			}
		})
	}
	doIsOpen := func() {
		a.ctrlRun("是否开机", FrameIsOpen, func(resp string) {
			switch {
			case strings.Contains(resp, "A1 10 01 00"):
				a.logCtrl("  -> 整机：已开机")
			case strings.Contains(resp, "A1 10 00 00"):
				a.logCtrl("  -> 整机：关机/待机")
			default:
				a.logCtrl("  -> 整机状态：未能判定")
			}
		})
	}

	// 构建「更多功能」下拉菜单
	menu := sb.Menu()
	menuAct := func(text string, fn func()) {
		act := walk.NewAction()
		_ = act.SetText(text)
		act.Triggered().Attach(fn)
		_ = menu.Actions().Add(act)
	}
	menuSep := func() { _ = menu.Actions().Add(walk.NewSeparatorAction()) }

	// 子菜单辅助
	menuSub := func(text string) *walk.Menu {
		sm, _ := walk.NewMenu()
		sa, _ := menu.Actions().AddMenu(sm)
		_ = sa.SetText(text)
		return sm
	}
	menuSubAct := func(sm *walk.Menu, text string, fn func()) {
		ac := walk.NewAction()
		_ = ac.SetText(text)
		ac.Triggered().Attach(fn)
		_ = sm.Actions().Add(ac)
	}

	menuAct("仅息屏（不睡眠）", doScreenOff)
	menuAct("亮屏", func() { a.ctrlRun("亮屏", FrameScreenOn, nil) })
	menuSep()

	// 电源
	pw := menuSub("电源")
	menuSubAct(pw, "整机关机…", doPowerOff)
	menuSubAct(pw, "开机", func() { a.ctrlRun("开机", FramePowerOn, nil) })
	menuSubAct(pw, "整机重启…", doReboot)

	// 状态查询
	qu := menuSub("状态查询")
	menuSubAct(qu, "查询屏幕状态", doStatus)
	menuSubAct(qu, "查询是否开机", doIsOpen)
	menuSubAct(qu, "查询软件版本", func() { a.ctrlRun("软件版本", FrameVersion, nil) })
	menuSubAct(qu, "查询整机ID", func() { a.ctrlRun("整机ID", FrameGetID, nil) })

	// 调节（音量/亮度）
	adj := menuSub("调节")
	menuSubAct(adj, "静音（音量0）", func() { a.ctrlRun("静音", buildByteFrame(0x07, 0), nil) })
	menuSubAct(adj, "音量 50", func() { a.ctrlRun("音量50", buildByteFrame(0x07, 50), nil) })
	menuSubAct(adj, "音量 100", func() { a.ctrlRun("音量100", buildByteFrame(0x07, 100), nil) })
	menuSubAct(adj, "亮度 30", func() { a.ctrlRun("亮度30", buildByteFrame(0x08, 30), nil) })
	menuSubAct(adj, "亮度 60", func() { a.ctrlRun("亮度60", buildByteFrame(0x08, 60), nil) })
	menuSubAct(adj, "亮度 100", func() { a.ctrlRun("亮度100", buildByteFrame(0x08, 100), nil) })

	// 通电状态
	pow := menuSub("通电状态")
	menuSubAct(pow, "上电自启（待实测）", func() { a.ctrlRun("通电状态=自启", buildPowerState(1), nil) })
	menuSubAct(pow, "上电待机（待实测）", func() { a.ctrlRun("通电状态=待机", buildPowerState(0), nil) })

	// 定时写入（可隐藏）
	a.mu.Lock()
	hideTimer := a.cfg.Link.HideManualTimer
	a.mu.Unlock()
	if !hideTimer {
		tm := menuSub("定时")
		menuSubAct(tm, "写入定时开机（按下方时间）", func() { a.writeRTC(1) })
		menuSubAct(tm, "写入定时关机（按下方时间）", func() { a.writeRTC(2) })
		menuSubAct(tm, "取消定时开机", func() { a.ctrlRun("取消定时开机", FrameCancelOn, nil) })
		menuSubAct(tm, "取消定时关机", func() { a.ctrlRun("取消定时关机", FrameCancelOff, nil) })
		menuSubAct(tm, "同步本机时间到整机", func() {
			now := time.Now()
			a.ctrlRun("设置日期", frameSetDate(now), nil)
			time.Sleep(150 * time.Millisecond)
			a.ctrlRun("设置时间", frameSetTime(now), nil)
		})
		menuSubAct(tm, "写入每日开机时间（按下方时间）", func() {
			hh, mm, ok := parseHHMM(a.timerTime.Text())
			if !ok {
				_ = walk.MsgBox(a.mw, "提示", "时间格式应为 HH:mm，例如 18:00", walk.MsgBoxIconWarning)
				return
			}
			a.ctrlRun("每日开机时间", frameDailyPower(true, hh, mm), nil)
		})
		menuSubAct(tm, "写入每日关机时间（按下方时间）", func() {
			hh, mm, ok := parseHHMM(a.timerTime.Text())
			if !ok {
				_ = walk.MsgBox(a.mw, "提示", "时间格式应为 HH:mm，例如 18:00", walk.MsgBoxIconWarning)
				return
			}
			a.ctrlRun("每日关机时间", frameDailyPower(false, hh, mm), nil)
		})
	}

	// 信源切换子菜单
	sigMenu, _ := walk.NewMenu()
	sigAction, _ := menu.Actions().AddMenu(sigMenu)
	_ = sigAction.SetText("信号源切换")
	addSig := func(text, kind string) {
		act := walk.NewAction()
		_ = act.SetText(text)
		act.Triggered().Attach(func() { a.ctrlRun("信源:"+text, frameSignalSource(kind), nil) })
		_ = sigMenu.Actions().Add(act)
	}
	addSig("OPS（内置电脑）", "OPS")
	addSig("HDMI1", "HDMI1")
	addSig("HDMI2", "HDMI2")
	addSig("VGA1", "VGA1")
	addSig("AV1", "AV1")
	addSig("YPbPr", "YPbPr")
	sigMenu.Actions().Add(walk.NewSeparatorAction())
	act := walk.NewAction()
	_ = act.SetText("查询当前信源")
	act.Triggered().Attach(func() { a.ctrlRun("查询当前信源", FrameSignalNow, nil) })
	_ = sigMenu.Actions().Add(act)

	// 图像模式子菜单
	modeMenu, _ := walk.NewMenu()
	modeAction, _ := menu.Actions().AddMenu(modeMenu)
	_ = modeAction.SetText("图像模式")
	addMode := func(text string, m byte) {
		act := walk.NewAction()
		_ = act.SetText(text)
		act.Triggered().Attach(func() { a.ctrlRun("图像模式:"+text, frameScreenMode(m), nil) })
		_ = modeMenu.Actions().Add(act)
	}
	addMode("标准", 0x07)
	addMode("运动", 0x08)
	addMode("录音", 0x09)
	addMode("电影", 0x0A)
	addMode("游戏", 0x0B)

	menuSep()
	a.addCustomMenu(menu, false)

	adjRow, _ := walk.NewComposite(page)
	adjRow.SetLayout(walk.NewHBoxLayout())
	vl, _ := walk.NewLabel(adjRow)
	vl.SetText("音量 0-100：")
	a.volNE, _ = walk.NewNumberEdit(adjRow)
	a.volNE.SetRange(0, 100)
	a.volNE.SetDecimals(0)
	a.volNE.SetValue(50)
	vb, _ := walk.NewPushButton(adjRow)
	vb.SetText("设置音量")
	vb.Clicked().Attach(func() { a.ctrlRun("音量", buildByteFrame(0x07, int(a.volNE.Value())), nil) })
	bl, _ := walk.NewLabel(adjRow)
	bl.SetText("亮度 0-100：")
	a.briNE, _ = walk.NewNumberEdit(adjRow)
	a.briNE.SetRange(0, 100)
	a.briNE.SetDecimals(0)
	a.briNE.SetValue(80)
	bb, _ := walk.NewPushButton(adjRow)
	bb.SetText("设置亮度")
	bb.Clicked().Attach(func() { a.ctrlRun("亮度", buildByteFrame(0x08, int(a.briNE.Value())), nil) })

	timerRow, _ := walk.NewComposite(page)
	timerRow.SetLayout(walk.NewHBoxLayout())
	tl, _ := walk.NewLabel(timerRow)
	tl.SetText("整机 RTC 定时：")
	a.timerType, _ = walk.NewComboBox(timerRow)
	_ = a.timerType.SetModel([]string{"定时开机", "定时关机"})
	_ = a.timerType.SetCurrentIndex(0)
	a.timerTime, _ = walk.NewLineEdit(timerRow)
	a.timerTime.SetText("18:00")
	ts, _ := walk.NewPushButton(timerRow)
	ts.SetText("写入定时")
	ts.Clicked().Attach(func() {
		typ := 1
		if a.timerType.CurrentIndex() == 1 {
			typ = 2
		}
		a.writeRTC(typ)
	})
	tc, _ := walk.NewPushButton(timerRow)
	tc.SetText("取消定时开/关机")
	tc.Clicked().Attach(func() {
		a.ctrlRun("取消定时开机", FrameCancelOn, nil)
		a.ctrlRun("取消定时关机", FrameCancelOff, nil)
	})

	rawRow, _ := walk.NewComposite(page)
	rawRow.SetLayout(walk.NewHBoxLayout())
	rl, _ := walk.NewLabel(rawRow)
	rl.SetText("自定义帧：")
	a.rawEdit, _ = walk.NewLineEdit(rawRow)
	a.rawEdit.SetText(FrameScreenOff)
	rb, _ := walk.NewPushButton(rawRow)
	rb.SetText("发送")
	rb.Clicked().Attach(func() {
		hex := strings.TrimSpace(a.rawEdit.Text())
		if _, err := hexToBytes(hex); err != nil {
			_ = walk.MsgBox(a.mw, "提示", "十六进制帧格式错误: "+err.Error(), walk.MsgBoxIconWarning)
			return
		}
		a.ctrlRun("自定义帧", hex, nil)
	})

	ll, _ := walk.NewLabel(page)
	ll.SetText("发送/回执日志：")
	a.mlog = newLogBox(page)
	a.mlog.SetMinMaxSize(walk.Size{Width: 0, Height: 200}, walk.Size{Width: 0, Height: 900})
	a.mlog.SetText("就绪。请先选择串口（或点自动探测）。")
}

// ==================== 定时调度 ====================

// schedPort 定时调度使用的串口：与手动控制选中端口保持一致（锁定端口）
func (a *App) schedPort() string {
	port := a.ctrlPort()
	if port == "" {
		a.mu.Lock()
		port = a.cfg.Engine.ManualPort
		if port == "" {
			port = a.cfg.Engine.FallbackSerial.COM
		}
		a.mu.Unlock()
	}
	return strings.TrimSpace(port)
}

func (a *App) schedSendFrame(hex string) bool {
	com := a.schedPort()
	if com == "" {
		a.logSched("未配置/未锁定串口，命令未下发（请先在“手动控制”页选择并锁定端口）")
		return false
	}
	if err := serialWrite(com, hex); err != nil {
		a.logSched("串口 %s 下发失败: %v", com, err)
		return false
	}
	a.logSched("串口 %s 下发成功: %s", com, hex)
	return true
}

func (a *App) execAction(act string) {
	if strings.TrimSpace(act) == "" {
		a.logSched("动作执行失败：动作键为空，请编辑该计划重新选择动作")
		return
	}
	if isCustomAction(act) {
		name := customActionName(act)
		a.logSched("动作[自定义命令:%s]启动", name)
		a.launchCustomByName(name)
		return
	}
	switch act {
	case "sleeppc":
		a.schedSendFrame(FrameScreenOff)
		a.logSched("动作[息屏+休眠]：2 秒后让 PC 睡眠")
		time.Sleep(2 * time.Second)
		a.managedSleep("定时动作")
		a.logSched("已触发系统睡眠（SetSuspendState）")
		return
	case "screenoff":
		a.schedSendFrame(FrameScreenOff)
		a.logSched("动作[仅息屏]完成")
		a.startScreenOffGuard()
		return
	case "syncclock":
		now := time.Now()
		a.schedSendFrame(frameSetDate(now))
		time.Sleep(300 * time.Millisecond)
		a.schedSendFrame(frameSetTime(now))
		a.logSched("动作[同步本机时间]完成")
		return
	}
	frame, ok := schedActionFrame(act)
	if !ok {
		a.logSched("未知动作: %s", act)
		return
	}
	if schedQueryActions[act] {
		// 查询类：读取串口回执并记录
		com := a.schedPort()
		if com == "" {
			a.logSched("未配置串口，查询动作[%s]未执行", act)
			return
		}
		go func() {
			resp, err := serialSend(com, frame)
			if err != nil {
				a.logSched("动作[%s]查询失败: %v", schedActionLabel(act), err)
				return
			}
			a.logSched("动作[%s]回执: %s", schedActionLabel(act), resp)
		}()
		return
	}
	if a.schedSendFrame(frame) {
		a.logSched("动作[%s]完成", schedActionLabel(act))
	}
}

// execActionMaybeConfirm 自动触发时按开关决定是否先弹 10 秒倒计时确认
func (a *App) execActionMaybeConfirm(act string) {
	a.mu.Lock()
	need := a.cfg.Engine.ConfirmCountdown
	a.mu.Unlock()
	if need {
		if !a.confirmCountdown(actionLabel(act)) {
			a.logSched("已取消本次自动执行：%s", actionLabel(act))
			return
		}
	}
	a.execAction(act)
}

// confirmCountdown 阻塞等待倒计时确认结果（在 UI 线程弹窗），返回 true=执行
func (a *App) confirmCountdown(actionName string) bool {
	ch := make(chan bool, 1)
	a.mw.Synchronize(func() {
		ch <- a.showCountdownDialog(actionName)
	})
	return <-ch
}

// showCountdownDialog 10 秒倒计时确认框，到点默认执行，可取消
func (a *App) showCountdownDialog(actionName string) bool {
	dlg, err := walk.NewDialog(a.mw)
	if err != nil {
		return true // 弹窗失败时不阻断自动执行
	}
	dlg.SetTitle("定时任务即将执行")
	dlg.SetLayout(walk.NewVBoxLayout())
	_ = dlg.SetSize(walk.Size{Width: 380, Height: 220})

	info, _ := walk.NewLabel(dlg)
	_ = info.SetText("即将执行任务：" + actionName)
	tip, _ := walk.NewLabel(dlg)
	_ = tip.SetText("倒计时结束后将自动执行；可点“取消本次”跳过。")
	num, _ := walk.NewLabel(dlg)
	_ = num.SetText("10")
	// 倒计时数字字号放大
	if f := num.Font(); f != nil {
		if big, err := walk.NewFont(f.Family(), f.PointSize()*2, f.Style()); err == nil {
			num.SetFont(big)
		}
	}

	row, _ := walk.NewComposite(dlg)
	row.SetLayout(walk.NewHBoxLayout())
	nowBtn, _ := walk.NewPushButton(row)
	nowBtn.SetText("立即执行")
	cancelBtn, _ := walk.NewPushButton(row)
	cancelBtn.SetText("取消本次")
	dlg.SetDefaultButton(nowBtn)
	dlg.SetCancelButton(cancelBtn)

	var dmu sync.Mutex
	decided := false
	result := true
	finish := func(ok bool) {
		dmu.Lock()
		if decided {
			dmu.Unlock()
			return
		}
		decided = true
		result = ok
		dmu.Unlock()
		if ok {
			dlg.Accept()
		} else {
			dlg.Cancel()
		}
	}
	nowBtn.Clicked().Attach(func() { finish(true) })
	cancelBtn.Clicked().Attach(func() { finish(false) })

	stop := make(chan struct{})
	go func() {
		n := 10
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			n--
			v := n
			dlg.Synchronize(func() {
				if v > 0 {
					_ = num.SetText(strconv.Itoa(v))
				}
			})
			if v <= 0 {
				finish(true)
				return
			}
		}
	}()

	code := dlg.Run()
	close(stop)
	if code == walk.DlgCmdOK {
		return true
	}
	return result
}

func (a *App) tick() {
	a.mu.Lock()
	if !a.cfg.Engine.Enabled {
		a.mu.Unlock()
		return
	}
	plans := make([]Plan, len(a.cfg.Schedule))
	copy(plans, a.cfg.Schedule)
	a.mu.Unlock()
	now := time.Now()
	today := now.Format("2006-01-02")
	changed := false
	for i := range plans {
		p := &plans[i]
		if !p.Enabled {
			continue
		}
		parts := strings.Split(p.Time, ":")
		if len(parts) != 2 && len(parts) != 3 {
			continue
		}
		ph, e1 := strconv.Atoi(parts[0])
		pm, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil {
			continue
		}
		ps := 0
		if len(parts) == 3 {
			if v, err := strconv.Atoi(parts[2]); err == nil {
				ps = v
			}
		}
		if now.Hour() == ph && now.Minute() == pm && now.Second() == ps && p.LastDate != today {
			if strings.TrimSpace(p.Action) == "" {
				a.logSched("触发计划 id=%d %s 失败：动作键为空（旧版本保存异常），请编辑该计划重新选择动作", p.ID, p.Time)
				p.LastDate = today
				changed = true
				continue
			}
			a.logSched("触发计划 id=%d %s %s", p.ID, p.Time, p.Action)
			a.execActionMaybeConfirm(p.Action)
			p.LastDate = today
			changed = true
		}
	}
	if changed {
		a.mu.Lock()
		for i := range a.cfg.Schedule {
			for j := range plans {
				if a.cfg.Schedule[i].ID == plans[j].ID {
					a.cfg.Schedule[i].LastDate = plans[j].LastDate
				}
			}
		}
		cfg := a.cfg
		a.mu.Unlock()
		if data, _ := json.MarshalIndent(cfg, "", "  "); data != nil {
			_ = os.WriteFile(a.cfgPath, data, 0644)
		}
	}
}

func (a *App) schedulerLoop() {
	a.tick()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.schedCancel:
			return
		case <-t.C:
			a.tick()
		}
	}
}

func (a *App) startScheduler() {
	a.schedMu.Lock()
	defer a.schedMu.Unlock()
	if a.running {
		return
	}
	if err := a.loadConfig(); err != nil {
		_ = walk.MsgBox(a.mw, "提示", "读取 plan.json 失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	a.schedCancel = make(chan struct{})
	a.running = true
	go a.schedulerLoop()
	a.logSched("调度已启动")
	a.updateStatusLocked()
}

func (a *App) stopScheduler() {
	a.schedMu.Lock()
	defer a.schedMu.Unlock()
	if !a.running {
		return
	}
	close(a.schedCancel)
	a.running = false
	a.logSched("调度已停止")
	a.updateStatusLocked()
}

// 调用方必须已持有 schedMu
func (a *App) updateStatusLocked() {
	r := a.running
	if a.statusBar != nil {
		a.mw.Synchronize(func() {
			if r {
				_ = a.statusBar.SetText("● 调度运行中")
				a.startBtn.SetEnabled(false)
				a.stopBtn.SetEnabled(true)
			} else {
				_ = a.statusBar.SetText("○ 调度未启动")
				a.startBtn.SetEnabled(true)
				a.stopBtn.SetEnabled(false)
			}
		})
	}
}

func (a *App) updateStatus() {
	a.schedMu.Lock()
	defer a.schedMu.Unlock()
	a.updateStatusLocked()
}

// ---- 表格模型 ----
type PlanModel struct {
	walk.TableModelBase
	app  *App
	rows []*Plan
}

func (m *PlanModel) RowCount() int { return len(m.rows) }
func (m *PlanModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.rows) {
		return ""
	}
	p := m.rows[row]
	switch col {
	case 1:
		return p.Time
	case 2:
		return actionLabel(p.Action)
	case 3:
		if p.LastDate == "" {
			return "-"
		}
		return p.LastDate
	}
	return ""
}
func (m *PlanModel) Checked(row int) bool {
	if row < 0 || row >= len(m.rows) {
		return false
	}
	return m.rows[row].Enabled
}
func (m *PlanModel) SetChecked(row int, checked bool) error {
	if row < 0 || row >= len(m.rows) {
		return nil
	}
	m.rows[row].Enabled = checked
	m.app.mu.Lock()
	plans := make([]Plan, len(m.app.allPlans))
	for i, p := range m.app.allPlans {
		plans[i] = *p
	}
	m.app.cfg.Schedule = plans
	m.app.mu.Unlock()
	_ = m.app.saveConfig()
	return nil
}

func (a *App) refreshPlans() {
	a.mu.Lock()
	var all []*Plan
	for i := range a.cfg.Schedule {
		cp := a.cfg.Schedule[i]
		all = append(all, &cp)
	}
	filter := a.planFilter
	a.mu.Unlock()
	a.allPlans = all
	if a.smodel != nil {
		var rows []*Plan
		for _, p := range all {
			if filter == "" || categoryOf(p.Action) == filter {
				rows = append(rows, p)
			}
		}
		a.smodel.rows = rows
		a.smodel.PublishRowsReset()
	}
}
func (a *App) persistPlans() {
	plans := make([]Plan, len(a.allPlans))
	for i, p := range a.allPlans {
		plans[i] = *p
	}
	a.mu.Lock()
	a.cfg.Schedule = plans
	a.mu.Unlock()
	_ = a.saveConfig()
	a.refreshPlans()
}
func (a *App) currentIndex() int {
	if a.table == nil {
		return -1
	}
	idx := a.table.CurrentIndex()
	if idx < 0 || idx >= len(a.smodel.rows) {
		return -1
	}
	return idx
}

func (a *App) addPlan() {
	act, tm, ok := a.runPlanDialog("添加计划", "")
	if !ok {
		return
	}
	maxID := 0
	for _, p := range a.allPlans {
		if p.ID > maxID {
			maxID = p.ID
		}
	}
	a.allPlans = append(a.allPlans, &Plan{ID: maxID + 1, Time: tm, Action: act, Enabled: true})
	a.persistPlans()
	a.logSched("添加计划 %s %s", tm, actionLabel(act))
}
func (a *App) editPlan() {
	idx := a.currentIndex()
	if idx < 0 {
		_ = walk.MsgBox(a.mw, "提示", "请先选中一条计划", walk.MsgBoxIconInformation)
		return
	}
	p := a.smodel.rows[idx]
	act, tm, ok := a.runPlanDialog("编辑计划", p.Action)
	if !ok {
		return
	}
	p.Action = act
	p.Time = tm
	a.persistPlans()
}
func (a *App) deletePlan() {
	idx := a.currentIndex()
	if idx < 0 {
		_ = walk.MsgBox(a.mw, "提示", "请先选中一条计划", walk.MsgBoxIconInformation)
		return
	}
	id := a.smodel.rows[idx].ID
	if walk.MsgBox(a.mw, "确认", "确定删除该计划？", walk.MsgBoxIconQuestion|walk.MsgBoxYesNo) != walk.DlgCmdYes {
		return
	}
	for i, p := range a.allPlans {
		if p.ID == id {
			a.allPlans = append(a.allPlans[:i], a.allPlans[i+1:]...)
			break
		}
	}
	a.persistPlans()
	a.logSched("删除计划 id=%d", id)
}

func (a *App) runPlanDialog(title, curAction string) (string, string, bool) {
	dlg, err := walk.NewDialog(a.mw)
	if err != nil {
		return "", "", false
	}
	dlg.SetTitle(title)
	dlg.SetLayout(walk.NewVBoxLayout())
	l1, _ := walk.NewLabel(dlg)
	l1.SetText("时间 (HH:mm:ss，精确到秒；秒可省略，默认 00)：")
	tm, _ := walk.NewLineEdit(dlg)
	tm.SetText("20:00:00")
	l2, _ := walk.NewLabel(dlg)
	l2.SetText("动作分类：")
	catCombo, _ := walk.NewComboBox(dlg)
	catLabels := make([]string, len(schedCategories))
	for i, c := range schedCategories {
		catLabels[i] = c.label
	}
	_ = catCombo.SetModel(catLabels)
	l3, _ := walk.NewLabel(dlg)
	l3.SetText("动作：")
	combo, _ := walk.NewComboBox(dlg)

	// 不再依赖 combo.Text() 回读（在对话框未显示/分类切换时可能为空），
	// 用变量显式跟踪当前分类与选中动作键。
	curCat := ""
	selectedKey := ""
	refreshActions := func(cat string) {
		curCat = cat
		labels := schedActionLabelsByCat(cat)
		_ = combo.SetModel(labels)
		idx := 0
		if curAction != "" && categoryOf(curAction) == cat {
			idx = schedIndexOfInCat(curAction, cat)
		}
		if idx < 0 || idx >= len(labels) {
			idx = 0
		}
		_ = combo.SetCurrentIndex(idx)
		if idx < len(labels) {
			selectedKey = schedKeyFromLabel(labels[idx])
		}
	}
	combo.CurrentIndexChanged().Attach(func() {
		i := combo.CurrentIndex()
		labels := schedActionLabelsByCat(curCat)
		if i >= 0 && i < len(labels) {
			selectedKey = schedKeyFromLabel(labels[i])
		}
	})
	catCombo.CurrentIndexChanged().Attach(func() {
		i := catCombo.CurrentIndex()
		if i < 0 || i >= len(schedCategories) {
			return
		}
		refreshActions(schedCategories[i].id)
	})
	// 初始分类：编辑时定位到该动作所在分类，否则默认息屏/休眠
	startCat := catSleep
	if curAction != "" {
		startCat = categoryOf(curAction)
	}
	for i, c := range schedCategories {
		if c.id == startCat {
			_ = catCombo.SetCurrentIndex(i)
			break
		}
	}
	refreshActions(startCat)
	brow, _ := walk.NewComposite(dlg)
	brow.SetLayout(walk.NewHBoxLayout())
	okPB, _ := walk.NewPushButton(brow)
	okPB.SetText("确定")
	ce, _ := walk.NewPushButton(brow)
	ce.SetText("取消")
	dlg.SetDefaultButton(okPB)
	dlg.SetCancelButton(ce)
	result := ""
	okPB.Clicked().Attach(func() {
		t := strings.TrimSpace(tm.Text())
		norm, ok := normalizePlanTime(t)
		if !ok {
			_ = walk.MsgBox(dlg, "提示", "时间格式应为 HH:mm:ss，例如 20:00:00", walk.MsgBoxIconWarning)
			return
		}
		if strings.TrimSpace(selectedKey) == "" {
			_ = walk.MsgBox(dlg, "提示", "请选择有效的动作（先选分类，再选动作）", walk.MsgBoxIconWarning)
			return
		}
		result = norm
		dlg.Accept()
	})
	ce.Clicked().Attach(func() { dlg.Cancel() })
	if dlg.Run() != walk.DlgCmdOK {
		return "", "", false
	}
	return selectedKey, result, true
}

func validHHMM(s string) bool {
	hh, mm, ok := parseHHMM(s)
	return ok && hh >= 0 && hh <= 23 && mm >= 0 && mm <= 59
}

// normalizePlanTime 校验并归一化为 HH:MM:SS；兼容旧的 HH:MM
func normalizePlanTime(s string) (string, bool) {
	p := strings.Split(strings.TrimSpace(s), ":")
	if len(p) != 2 && len(p) != 3 {
		return "", false
	}
	h, e1 := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return "", false
	}
	sec := 0
	if len(p) == 3 {
		v, err := strconv.Atoi(p[2])
		if err != nil || v < 0 || v > 59 {
			return "", false
		}
		sec = v
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, sec), true
}
func parseHHMM(s string) (int, int, bool) {
	p := strings.Split(strings.TrimSpace(s), ":")
	if len(p) != 2 {
		return 0, 0, false
	}
	h, e1 := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return h, m, true
}

func (a *App) buildScheduleTab(page *walk.TabPage) {
	page.SetLayout(newTabPageLayout())
	sv, _ := walk.NewScrollView(page)
	sv.SetScrollbars(false, true)
	sv.SetLayout(walk.NewVBoxLayout())

	engRow, _ := walk.NewComposite(sv)
	engRow.SetLayout(walk.NewHBoxLayout())
	a.engineCk, _ = walk.NewCheckBox(engRow)
	a.engineCk.SetText("启用定时（直接走与手动控制相同的锁定串口，不依赖原厂网关）")
	a.engineCk.SetChecked(a.cfg.Engine.Enabled)
	es, _ := walk.NewPushButton(engRow)
	es.SetText("应用")
	es.Clicked().Attach(func() {
		a.mu.Lock()
		a.cfg.Engine.Enabled = a.engineCk.Checked()
		a.mu.Unlock()
		_ = a.saveConfig()
		_ = walk.MsgBox(a.mw, "提示", "定时开关已保存", walk.MsgBoxIconInformation)
	})

	// 倒计时确认开关
	cdRow, _ := walk.NewComposite(sv)
	cdRow.SetLayout(walk.NewHBoxLayout())
	a.confirmCk, _ = walk.NewCheckBox(cdRow)
	a.confirmCk.SetText("自动执行前 10 秒倒计时确认（到点弹窗，可取消；仅对自动定时生效）")
	a.confirmCk.SetChecked(a.cfg.Engine.ConfirmCountdown)
	cdBtn, _ := walk.NewPushButton(cdRow)
	cdBtn.SetText("保存开关")
	saveConfirm := func() {
		a.mu.Lock()
		a.cfg.Engine.ConfirmCountdown = a.confirmCk.Checked()
		a.mu.Unlock()
		_ = a.saveConfig()
		a.logSched("倒计时确认开关：%v", a.confirmCk.Checked())
	}
	a.confirmCk.Clicked().Attach(saveConfirm)
	cdBtn.Clicked().Attach(func() {
		saveConfirm()
		_ = walk.MsgBox(a.mw, "提示", "已保存", walk.MsgBoxIconInformation)
	})

	// 自启 + 备用串口
	optRow, _ := walk.NewComposite(sv)
	optRow.SetLayout(walk.NewVBoxLayout())
	autoRow, _ := walk.NewComposite(optRow)
	autoRow.SetLayout(walk.NewHBoxLayout())
	a.autoCk, _ = walk.NewCheckBox(autoRow)
	a.autoCk.SetText("开机自启（登录后自动启动本程序并最小化到托盘、开始调度）")
	a.autoCk.SetToolTipText("写入当前用户开机启动项，无需管理员权限")
	if on, _ := isAutoStart(); on {
		a.autoCk.SetChecked(true)
	}
	comRow, _ := walk.NewComposite(optRow)
	comRow.SetLayout(walk.NewHBoxLayout())
	cl, _ := walk.NewLabel(comRow)
	cl.SetText("备用串口（仅在未锁定端口时使用；定时与手动默认都用锁定端口）：")
	comLe, _ := walk.NewLineEdit(comRow)
	comLe.SetText(a.cfg.Engine.FallbackSerial.COM)
	comLe.SetMaxLength(6)
	a.autoCk.Clicked().Attach(func() {
		exe, _ := os.Executable()
		if err := setAutoStart(a.autoCk.Checked(), exe); err != nil {
			a.logSched("设置开机自启失败: %v", err)
			_ = walk.MsgBox(a.mw, "提示", "设置开机自启失败: "+err.Error(), walk.MsgBoxIconError)
		} else {
			a.logSched("开机自启：%v", a.autoCk.Checked())
		}
	})
	comLe.TextChanged().Attach(func() {
		a.mu.Lock()
		a.cfg.Engine.FallbackSerial.COM = strings.TrimSpace(comLe.Text())
		a.cfg.Engine.FallbackSerial.Baud = serialBaud
		a.mu.Unlock()
		_ = a.saveConfig()
	})

	tt, _ := walk.NewLabel(sv)
	tt.SetText("定时计划（勾选=启用；可按分类筛选显示）：")

	// 分类筛选
	filterRow, _ := walk.NewComposite(sv)
	filterRow.SetLayout(walk.NewHBoxLayout())
	for _, c := range schedCategories {
		cid := c.id
		rb, _ := walk.NewRadioButton(filterRow)
		rb.SetText(c.label)
		if cid == "" {
			rb.SetChecked(true)
		}
		rb.Clicked().Attach(func() {
			a.planFilter = cid
			a.refreshPlans()
		})
	}

	a.smodel = &PlanModel{app: a}
	a.refreshPlans()
	tbl, _ := walk.NewTableView(sv)
	tbl.SetCheckBoxes(true)
	tbl.SetMinMaxSize(walk.Size{Width: 0, Height: 260}, walk.Size{Width: 0, Height: 900})
	addCol := func(title string, w int) {
		c := walk.NewTableViewColumn()
		_ = c.SetTitle(title)
		_ = c.SetWidth(w)
		_ = tbl.Columns().Add(c)
	}
	addCol("启用", 50)
	addCol("时间(HH:MM:SS)", 110)
	addCol("动作", 250)
	addCol("最近执行日期", 150)
	_ = tbl.SetLastColumnStretched(true)
	_ = tbl.SetModel(a.smodel)
	a.table = tbl

	btnRow, _ := walk.NewComposite(sv)
	btnRow.SetLayout(walk.NewHBoxLayout())
	mk := func(text string, fn func()) *walk.PushButton {
		b, _ := walk.NewPushButton(btnRow)
		b.SetText(text)
		b.Clicked().Attach(fn)
		return b
	}
	mk("添加计划", a.addPlan)
	mk("编辑", a.editPlan)
	mk("删除", a.deletePlan)
	mk("立即执行所选", func() {
		idx := a.currentIndex()
		if idx < 0 {
			_ = walk.MsgBox(a.mw, "提示", "请先选中一条计划", walk.MsgBoxIconInformation)
			return
		}
		p := a.smodel.rows[idx]
		a.logSched("手动执行: %s", actionLabel(p.Action))
		go a.execAction(p.Action)
	})
	a.startBtn = mk("启动调度", a.startScheduler)
	a.stopBtn = mk("停止调度", a.stopScheduler)
	a.stopBtn.SetEnabled(false)

	sl, _ := walk.NewLabel(sv)
	sl.SetText("运行日志：")
	a.slog = newLogBox(sv)
	a.slog.SetMinMaxSize(walk.Size{Width: 0, Height: 180}, walk.Size{Width: 0, Height: 900})
	a.slog.SetText("就绪。")
}

// ==================== 主窗口 ====================

func (a *App) buildGUI() (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("界面初始化异常: %v", r)
		}
	}()
	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	a.mw = mw
	mw.SetTitle("海信一体机 助手（手动控制 + 定时计划）")
	mw.SetLayout(walk.NewVBoxLayout())
	_ = mw.SetSize(walk.Size{Width: 820, Height: 820})

	tab, err := walk.NewTabWidget(mw)
	if err != nil {
		return err
	}
	a.tabs = tab

	pageManual, err := walk.NewTabPage()
	if err != nil {
		return err
	}
	pageManual.SetTitle("手动控制")
	if err := tab.Pages().Add(pageManual); err != nil {
		return err
	}
	a.buildManualTab(pageManual)

	pageSched, err := walk.NewTabPage()
	if err != nil {
		return err
	}
	pageSched.SetTitle("定时计划")
	if err := tab.Pages().Add(pageSched); err != nil {
		return err
	}
	a.buildScheduleTab(pageSched)

	pageLink, err := walk.NewTabPage()
	if err != nil {
		return err
	}
	pageLink.SetTitle("联动设置")
	if err := tab.Pages().Add(pageLink); err != nil {
		return err
	}
	a.buildLinkTab(pageLink)

	pagePersonal, err := walk.NewTabPage()
	if err != nil {
		return err
	}
	pagePersonal.SetTitle("个性化与工具")
	if err := tab.Pages().Add(pagePersonal); err != nil {
		return err
	}
	a.buildPersonalTab(pagePersonal)

	sb := walk.NewStatusBarItem()
	if err := mw.StatusBar().Items().Add(sb); err != nil {
		return err
	}
	a.statusBar = sb
	_ = a.statusBar.SetText("○ 调度未启动")
	return nil
}

func alertBox(title, msg string) {
	_, _ = windows.MessageBox(0, windows.StringToUTF16Ptr(msg),
		windows.StringToUTF16Ptr(title), 0x00000010)
}

func main() {
	runtime.LockOSThread()

	// 命令行模式：第三方程序/脚本调用，无头执行，不走单实例与 GUI
	flags := cliFlags(os.Args[1:])
	if isCLIMode(flags) {
		exe, _ := os.Executable()
		dir := filepath.Dir(exe)
		headless := &App{dir: dir, cfgPath: filepath.Join(dir, "plan.json")}
		headless.cfg = Config{
			Engine: Engine{FallbackSerial: FallbackSerial{COM: "COM1", Baud: serialBaud}},
			Link:   Link{Hotkeys: defaultHotkeys()},
		}
		_ = headless.loadConfig()
		os.Exit(runCLI(flags, &headless.cfg))
	}

	// 单实例：避免多开导致全局热键互相占用、串口被抢占
	if _, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr("Local\\HisenseAssistantSingleInstance")); err == windows.ERROR_ALREADY_EXISTS {
		alertBox("助手已在运行", "海信一体机助手已经在运行中（请勿重复打开；后台/托盘模式下可用 Ctrl+Alt+Shift+M 呼出）。")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	dir := filepath.Dir(exe)
	a := &App{
		dir: dir, cfgPath: filepath.Join(dir, "plan.json"),
		schedLogPath: filepath.Join(dir, "scheduler.log"),
		ctrlLogPath:  filepath.Join(dir, "control.log"),
		crashPath:    filepath.Join(dir, "crash.log"),
	}
	a.cfg = Config{
		Engine: Engine{
			Enabled:          true,
			ConfirmCountdown: true,
			GatewayPath:      "",
			FallbackSerial:   FallbackSerial{COM: "COM1", Baud: serialBaud},
		},
		Link: Link{
			HotkeysEnabled:      true,
			Hotkeys:             defaultHotkeys(),
			SleepKeyScreenOff:   false,
			SleepEventScreenOff: true,
			SleepKeyBlockSleep:  true,
			GuardSleep:          false,
			GuardWaitSec:        60,
			GuardAppBlock:       false,
			WakeBlock:           false,
			Skin:                "classic",
			FontSize:            9,
			Opacity:             100,
			WinShutdownLink:     true,
			StartToTray:         false,
			Background:          false,
		},
	}
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			_ = os.WriteFile(a.crashPath, []byte(fmt.Sprintf("FATAL: %v\n%s\n", r, buf[:n])), 0644)
			alertBox("助手启动失败", fmt.Sprintf("程序异常退出，详见 crash.log\n\n%v", r))
		}
	}()

	cfgErr := a.loadConfig()
	if cfgErr == nil {
		a.logSched("已加载配置 %s", a.cfgPath)
	} else {
		a.logSched("plan.json 不存在/损坏，使用内置默认配置: %v", cfgErr)
		// 从压缩包临时目录直接运行时提示用户解压
		low := strings.ToLower(dir)
		if strings.Contains(low, "temp") || strings.Contains(low, "$") || strings.Contains(low, "360zip") {
			alertBox("请先解压后再运行",
				"检测到程序正在压缩包临时目录中运行，读取不到 plan.json，设置也无法保存。\n\n请把整个文件夹解压到固定位置（如桌面或 D:\\海信助手）后，再运行 海信一体机助手.exe。")
		}
	}
	if len(a.cfg.Link.Hotkeys) == 0 {
		a.cfg.Link.Hotkeys = defaultHotkeys()
	}
	if err := a.buildGUI(); err != nil {
		alertBox("助手启动失败", "界面初始化失败: "+err.Error())
		return
	}
	// 应用已保存的皮肤与字体
	a.mu.Lock()
	skinID := a.cfg.Link.Skin
	fontSize := a.cfg.Link.FontSize
	a.mu.Unlock()
	if skinID != "" && skinID != "classic" || fontSize > 0 {
		a.applySkin(skinID, fontSize)
	}
	a.buildTray()
	a.refreshPorts()
	a.refreshPlans()
	a.updateStatus()
	// 启动全局快捷键/休眠键监听窗口（独立线程消息泵）
	a.hub = newHotkeyHub(a)
	go a.hub.run()

	// 启动模式：完全后台（无窗口无托盘）/ 最小化到托盘（含 --minimized 自启）/ 正常显示
	startHidden := false
	for _, arg := range os.Args[1:] {
		if strings.EqualFold(strings.TrimSpace(arg), "--minimized") {
			startHidden = true
		}
	}
	if a.cfg.Link.Background {
		a.hideToTray(true)
	} else if startHidden || a.cfg.Link.StartToTray {
		a.hideToTray(false)
	} else {
		a.mw.Show()
	}
	// 若已设置开机自启，则登录后自动开始调度
	if on, _ := isAutoStart(); on {
		a.logSched("检测到开机自启，自动开始调度")
		a.startScheduler()
	}
	// 息屏守护全局启动：程序运行期间即常驻轮询（无论是否有人息屏），
	// 检测到整机已息屏但 PC 未休眠则自动休眠（开关在联动/隐藏设置中）
	a.startScreenOffGuard()
	a.mw.Run()
}
