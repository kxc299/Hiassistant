```markdown
# Hiassistant 海信一体机助手
面向海信教育一体机的 Windows 串口控制助手，纯 Go 编写、单 EXE、绿色免安装，无联网、无网关依赖。

## ✨ 功能清单
- 手动串口控制：开机、关机、息屏、亮屏、切换信号源，串口收发日志查看
- 定时计划任务：支持单次、每日、每周定时下发串口指令
- 系统联动：Windows 关机/重启时自动关闭一体机，系统休眠自动息屏
- 界面个性化：多套内置皮肤、自定义背景图片、字体大小、窗口透明度
- 扩展能力：自定义外部命令、托盘后台运行、命令行调用接口

## 🖥️ 运行环境
- Windows 10 / Windows 11（64位）
- 一体机通过 RS232 串口连接，默认参数：COM1，9600 8N1
- 没有对应串口时会提示打开串口失败，属于正常现象

## 🛠️ 编译方式
```bash
go build -ldflags="-H windowsgui"
```
编译时项目内 `app.manifest`、`rsrc.syso` 需要一同保留，用于高DPI适配。

## 📖 命令行调用示例
```bash
# 查看支持动作列表
Hiassistant.exe --list-actions
# 息屏
Hiassistant.exe --action screenoff
# 关机（指定端口）
Hiassistant.exe --action poweroff --port COM1
# 发送自定义HEX帧
Hiassistant.exe --frame "DD FF 00 07 C1 FF 00 12 01 00 2A BB CC"
# 运行自定义命令
Hiassistant.exe --run "自定义命令名"
```

## ⚠️ 重要说明
1. 程序设置保存在同目录 `plan.json`
2. 程序未做数字签名，部分杀毒软件会误报，可自行添加白名单或源码编译
3. 仅用于许可的设备管理，自动关机操作会强制中断设备内容，请提前保存资料
4. 本工具基于 RS232 串口协议控制，不需要网络、不需要网关

