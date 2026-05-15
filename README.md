# di

`di` 是一个可断开、可重新进入的终端会话工具。它用 Go 自己管理 PTY 和 Unix socket，不依赖 `dtach`。

## 安装

```sh
git clone git@github.com:whoamihappyhacking/di.git
cd di
go build -o di .
./di install
```

安装后：

```text
~/.local/bin/d
~/.local/bin/di -> ~/.local/bin/d
```

确保 `~/.local/bin` 在 `PATH` 里。

## 用法

启动或进入一个会话：

```sh
d codex --yolo
```

断开 attach，后端命令继续运行：

```text
Ctrl-]
```

选择已有会话：

```sh
di
```

列出会话：

```sh
d --list
```

从另一个终端断开某个 attach 客户端：

```sh
d --detach codex---yolo
```

临时修改 detach 快捷键：

```sh
D_DETACH='^B' di
```

## 构建

Linux/macOS 都支持。

```sh
go build -o di .
GOOS=darwin GOARCH=arm64 go build -o di-darwin-arm64 .
GOOS=darwin GOARCH=amd64 go build -o di-darwin-amd64 .
```

## 说明

`di` 解决的是“终端断开后重新进入”的问题，不是 checkpoint 工具；它不会保存进程内存、文件系统快照或网络连接状态。
