# ForzaNext Studio v3

面向《极限竞速：地平线》（Forza Horizon 4 / 5 / 6 UDP 323/324B 遥测）打造的 **单文件双模式 1 kHz 驾驶触觉引擎 + Flydigi APEX 6 / K6 遥测灯带工作台**（Go 纯标准库，零外部依赖）。

## 核心架构
- **娱乐模式（Next 状态推演引擎 · `internal/engine`）**：
  - 原生 Go 1 kHz / 8 ms 实时合成，直接下发 33 字节 HID `0x53 / 0x57` 报文（[hid_windows.go](internal/hid/hid_windows.go)）；
  - **一车一性格 & 7 大引擎家族（18 套动态模板）**：根据实时 UDP 缸数、驱动、PI、在线反推物理悬挂行程（cm）与峰值增压自动匹配，横跨 `16–156 Hz` 与 `36%–165%` 振幅差，内置防打滑真实红线自学习（[personality.go](internal/engine/personality.go)）；
  - **四轮胎直驱推头 / 甩尾**：`FL/FR/RL/RR` 载荷加权滑移分别映射左右握把与左右扳机，四轮大角度漂移触发 `0x9E` 双扳机同步（[synth.go](internal/engine/synth.go)）；
  - **高低速 360° 撞击 & 三特征定向护栏刮擦**：3–80 km/h 低速碰擦与高速重撞双层上升沿捕获（无撞后余震），左右侧撞与护栏实现 100% vs 0% 单侧隔离；
  - **6 种路面质感 + 涉水飞坡**：柏油、土路随机石粒、重砂石、草雪、随速锯齿路肩、大小飞坡落地与入水波浪阻力。
- **专业模式（V4 官方引擎退路 · `internal/v4bridge`）**：
  - 内置官方 V4 运行时（`internal/assets/v4_bundle.tar.gz`）与 **11 套社区精选预设**（`internal/assets/presets/`），切换模式时按需解压至 `%LOCALAPPDATA%\ForzaNext\v4_runtime\` 并无缝转发 UDP `5300 → 15300`。
- **Flydigi APEX 6 / K6 遥测灯带（`internal/lighting`）**：
  - 原生 Win32 Overlapped HID `0xA3 / 0xA4` 并行写入 `0xFFA0` 通道，与 1 kHz 震动和 V4 专业模式完全共存；
  - 优先级：`ABS 冰蓝爆闪 > 急刹纯红变频爆闪 > 红线红白爆闪 > 5 段平滑转速渐变`；
  - 首次连接强制备份原灯效 `RAM 0x04`，暂停/非驾驶、关闭开关、点网页「安全退出」或关闭控制台窗口均自动精确恢复原灯效。
- **单文件打包（`internal/assets`）**：
  - 通过 `//go:embed` 全量嵌入前端页面、11 套 V4 预设与压缩 V4 运行时，编译产物为单一 `ForzaNext.exe`（约 7.7 MB）。

## 编译与测试

需要 Go 1.23+（Windows x64）：

```powershell
# 运行单元测试（如有真实录制包可设置环境变量 FORZA_RECORDINGS_DIR 跑回放回归）
go test -v ./...

# 一键构建单文件 ForzaNext.exe
.\build.cmd
# 或手动编译：
go build -ldflags "-s -w" -o dist\ForzaNext.exe .
```

## 运行与游戏配置
1. 先退出系统托盘中的飞智空间站 / 旧版 Flydigi Engine，避免占用 UDP `5300` 或手柄 HID 通道。
2. 直接双击编译好的 `ForzaNext.exe`，自动启动工作台并打开 `http://127.0.0.1:8300/`。
3. 在《极限竞速：地平线》数据输出设置中填写 `127.0.0.1`，端口 `5300`。
4. 退出时点击网页右上角 **「安全退出」** 或关闭控制台窗口即可自动恢复手柄原灯效。
