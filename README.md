# di

`di` 是一个很小的可断开终端会话工具，目标是把常用命令放进可重新 attach 的 PTY 里。

它的使用方式类似：

```sh
d codex --yolo
```

按 `Ctrl-]` 断开后，命令仍在后台运行。之后可以用：

```sh
di
```

通过 `fzf` 选择并重新进入会话。

## 特性

- `d <command>`：启动或进入同名会话
- `di`：用 `fzf` 选择已有会话
- `d --list`：列出会话
- `d --detach <name>`：从外部断开某个 attach 客户端
- `d install`：从源码安装到 `~/.local/bin`
- 默认保留终端鼠标选择/复制能力
- 内置静态 Linux x86_64 `dtach`，用户不需要额外安装 `dtach`

如果系统里已有 `dtach`，会优先使用系统版本；否则会自动释放内置版本到：

```text
~/.cache/di/
```

## 安装

需要 Go：

```sh
git clone git@github.com:whoamihappyhacking/di.git
cd di
go build -o di .
./di install
```

安装后会生成：

```text
~/.local/bin/d
~/.local/bin/di -> ~/.local/bin/d
```

确保 `~/.local/bin` 在 `PATH` 里。

## 用法

启动一个可断开的会话：

```sh
d codex --yolo
```

断开当前 attach：

```text
Ctrl-]
```

重新进入：

```sh
di
```

列出已有会话：

```sh
d --list
```

直接进入同名会话：

```sh
d codex --yolo
```

如果该命令对应的 socket 已存在，`d` 会直接 attach；不存在时才会创建新会话。

## 鼠标

默认不启用鼠标事件穿透，这样终端里可以正常鼠标选中文本并复制。

如果某次需要把鼠标事件传给 TUI，可以显式打开：

```sh
D_MOUSE=1 di
```

或：

```sh
D_MOUSE=1 d codex --yolo
```

## 会话名

会话名根据命令自动生成，例如：

```sh
d codex --yolo
```

会生成类似：

```text
codex---yolo
```

所以可以从另一个终端断开 attach 客户端：

```sh
d --detach codex---yolo
```

## 说明

`di` 不是进程 checkpoint 工具。它解决的是“终端断开/重新进入”的问题，不负责保存进程内存、文件系统快照或网络连接状态。
