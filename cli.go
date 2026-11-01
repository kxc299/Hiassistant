// 命令行模式：供第三方程序/脚本调用，无头执行动作或自定义 HEX 帧
package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// cliFlags 解析命令行参数，支持 --flag value 与 --flag=value
func cliFlags(args []string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		if !strings.HasPrefix(a, "-") {
			continue
		}
		a = strings.TrimLeft(a, "-")
		if eq := strings.Index(a, "="); eq >= 0 {
			m[strings.ToLower(a[:eq])] = a[eq+1:]
			continue
		}
		key := strings.ToLower(a)
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			m[key] = args[i+1]
			i++
		} else {
			m[key] = "true"
		}
	}
	return m
}

// isCLIMode 是否为命令行调用（非 GUI 启动）
func isCLIMode(flags map[string]string) bool {
	for _, k := range []string{"action", "frame", "send", "run", "list-actions", "help", "h"} {
		if _, ok := flags[k]; ok {
			return true
		}
	}
	return false
}

func printUsage() {
	fmt.Println(`海信一体机助手 - 命令行用法（第三方程序/脚本可直接调用）：
  --list-actions              列出所有内置动作键
  --action <动作键> [--port COMx]
      执行内置动作，如 poweroff / poweron / reboot / screenoff / screenon /
      sleeppc / status / syncclock / src_hdmi1 / mode_game 等
  --frame "<HEX 帧>" [--port COMx]
      发送自定义十六进制帧，例如 --frame "DD FF 00 07 C1 FF 00 12 01 01 2B BB CC"
  --run <自定义命令名>         运行“个性化与工具”页中配置的外部程序
  --port <COMx>               指定串口；缺省取 plan.json 锁定端口，再缺省 COM1
示例：
  海信一体机助手.exe --action screenoff
  海信一体机助手.exe --action poweroff --port COM1
  海信一体机助手.exe --frame "DD FF 00 07 C1 FF 00 12 01 00 2A BB CC"`)
}

// runCLI 执行命令行模式，返回退出码
func runCLI(flags map[string]string, cfg *Config) int {
	if v, ok := flags["help"]; ok && v == "true" || flags["h"] == "true" {
		printUsage()
		return 0
	}
	if v, ok := flags["list-actions"]; ok && v == "true" {
		for _, a := range schedActions {
			fmt.Printf("%-14s %s\n", a.key, a.label)
		}
		return 0
	}

	// 串口解析：--port > 配置锁定端口 > 备用 COM1
	port := strings.TrimSpace(flags["port"])
	if port == "" {
		port = cfg.Engine.ManualPort
	}
	if port == "" {
		port = cfg.Engine.FallbackSerial.COM
	}
	if port == "" {
		port = "COM1"
	}

	// 运行自定义外部命令
	if name, ok := flags["run"]; ok && name != "true" {
		return runCustomByName(cfg, name)
	}

	// 自定义 HEX 帧
	if frame, ok := flags["frame"]; ok && frame != "true" {
		return sendHeadlessFrame(port, frame)
	}
	if frame, ok := flags["send"]; ok && frame != "true" {
		return sendHeadlessFrame(port, frame)
	}

	// 内置动作
	if act, ok := flags["action"]; ok && act != "true" {
		return runHeadlessAction(port, act)
	}
	printUsage()
	return 1
}

func sendHeadlessFrame(port, frame string) int {
	if _, err := hexToBytes(frame); err != nil {
		fmt.Fprintf(os.Stderr, "HEX 帧格式错误: %v\n", err)
		return 1
	}
	resp, err := serialSend(port, frame)
	if err != nil {
		fmt.Fprintf(os.Stderr, "发送失败: %v\n", err)
		return 1
	}
	if resp == "" {
		fmt.Println("发送成功（无回执）")
		return 0
	}
	fmt.Println("回执:", resp)
	return 0
}

func runHeadlessAction(port, act string) int {
	act = strings.TrimSpace(act)
	if act == "" {
		fmt.Fprintln(os.Stderr, "动作键为空")
		return 1
	}
	switch act {
	case "screenoff":
		return writeHeadless(port, FrameScreenOff, "仅息屏")
	case "screenon":
		return writeHeadless(port, FrameScreenOn, "亮屏")
	case "sleeppc":
		if code := writeHeadless(port, FrameScreenOff, "息屏"); code != 0 {
			return code
		}
		time.Sleep(1500 * time.Millisecond)
		sleepPC()
		fmt.Println("已触发 PC 睡眠")
		return 0
	case "syncclock":
		now := time.Now()
		if code := writeHeadless(port, frameSetDate(now), "设置日期"); code != 0 {
			return code
		}
		time.Sleep(300 * time.Millisecond)
		return writeHeadless(port, frameSetTime(now), "设置时间")
	}
	frame, ok := schedActionFrame(act)
	if !ok {
		fmt.Fprintf(os.Stderr, "未知动作: %s（用 --list-actions 查看）\n", act)
		return 1
	}
	if schedQueryActions[act] {
		resp, err := serialSend(port, frame)
		if err != nil {
			fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
			return 1
		}
		fmt.Println("回执:", resp)
		return 0
	}
	return writeHeadless(port, frame, schedActionLabel(act))
}

func writeHeadless(port, frame, name string) int {
	if err := serialWrite(port, frame); err != nil {
		fmt.Fprintf(os.Stderr, "%s失败: %v\n", name, err)
		return 1
	}
	fmt.Printf("%s成功: %s\n", name, frame)
	return 0
}

// runCustomByName 按名称运行已配置的自定义命令
func runCustomByName(cfg *Config, name string) int {
	for _, c := range cfg.CustomCommands {
		if c.Name == name {
			args := strings.Fields(c.Args)
			cmd := newOSCmd(c.Path, args)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "启动自定义命令失败: %v\n", err)
				return 1
			}
			fmt.Println("已启动自定义命令:", name)
			return 0
		}
	}
	fmt.Fprintf(os.Stderr, "未找到自定义命令: %s\n", name)
	return 1
}
