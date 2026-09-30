# di

[项目主页](https://whoamihappyhacking.github.io/di/)

`di` 是一个可断开、可重新进入的终端会话工具。它用 Go 自己管理 PTY 和 Unix socket，不依赖 `dtach`。

## 依赖

`di` 使用 `fzf` 选择已有会话；请先确保 `fzf` 已安装并在 `PATH` 中。

启动新会话和列出会话不依赖 `fzf`：

```sh
d <command> [args...]
d --list
```

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

查看命令说明：

```sh
d --help
di --help
```

启动一个会话：

```sh
d codex --yolo
```

命令名支持 `$SHELL` 启动配置中的 **bash** 和 **zsh** 别名，后续参数会按原样传给命令：

```sh
d app          # alias app='app -f /etc/app.conf' → 实际执行 app -f /etc/app.conf
d app -x       # → app -f /etc/app.conf -x
```

断开 attach，后端命令继续运行：

```text
Ctrl-]
```

鼠标滚轮不会转发给后端程序，方便用终端自己的滚屏查看历史输出。

选择已有会话：

```sh
di
```

`di` 会显示每个会话启动时的目录、命令和最近的终端画面；用方向键切换候选项时，下方会同步更新全宽预览，按 Enter attach 到选中的会话。预览会解析 Agent 等全屏终端程序的重绘控制序列，只显示最后一帧画面。即使在同一个目录里重复执行同一个命令，也会创建不同的 session。

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
每次启动新会话都会重新读取 shell 别名，修改被 `source` 的文件或插件后，下次启动即可生效。读取速度取决于 shell 启动配置；别名探测最多等待约 5 秒。别名会交给对应的 shell 执行，支持环境变量赋值、引号、嵌套别名和管道，额外参数中的空格或 `$()` 等内容不会被重新解释。仅在当前终端临时定义的别名需要先写入 shell 启动配置。

## 项目介绍网页

在线访问：[di 项目介绍页](https://whoamihappyhacking.github.io/di/)。推送 `website/` 更新到 `main` 后，GitHub Actions 会自动部署到 GitHub Pages。

本地介绍页位于 `website/`，包含断开、会话预览和重新连接的浏览器交互演示。演示不执行真实终端命令。

```sh
python3 -m http.server 7789 --bind 0.0.0.0 --directory website
```

启动后访问 <http://localhost:7789/>。
