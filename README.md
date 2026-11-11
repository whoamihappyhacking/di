# di

**给 TUI 智能体的中断与任意恢复。**

让 Claude Code、Codex 这类智能体在后台继续运行：随时离开，从任何终端回来；机器重启了，也能在原目录接着来。

[项目主页](https://whoamihappyhacking.github.io/di/)

## 只需要记住两个命令

```sh
d claude    # 启动：在命令前加 d，它就跑在一个可以断开的会话里
di          # 回来：选一个会话，回到离开时的样子
```

在会话里按 `Ctrl-]` 离开，智能体照常工作；按 `Ctrl-\` 切到另一个会话。

## 功能

- **原样直通**：会话里的输出原样到达你的终端，滚屏、鼠标、颜色、快捷键都和直接运行一样。
- **回来即现场**：重新进入时，历史滚屏、当前画面、光标和终端模式都还在。
- **一眼看清状态**：运行中、待处理、有更新、空闲、已退出，需要你处理的排在最前。
- **重启也不丢**：机器重启后，没跑完的会话显示为“已中断”，按 `Enter` 在原目录重新运行。

## 界面

运行 `di`：上面是会话列表，下面是选中会话的画面预览。

```text
di>                                                                 5/5
  Enter 进入/恢复 · Ctrl-O 恢复到目录 · Ctrl-X 结束/删除 · Esc 退出
▌ ◆ 待处理 ~/api        claude       允许修改 session.go？           2m
  ◆ 有更新 ~/web        codex        完成：登录页重构               12m
  ● 运行中 ~/di         claude       Running tests…                  3s
  ○ 空闲   ~/notes      zsh                                          1h
  ⟲ 已中断 ~/infra      codex --yolo 系统重启前在运行                2h
───────────────────────────────────────────────────────────────────────
◆ 待处理 claude · ~/api · 2m ago

  ⏺ 准备修改 internal/store/session.go：
    把会话目录从 /tmp 挪到 XDG_STATE_HOME。

  允许这次修改吗？
  ❯ 1. 允许
    2. 不允许，告诉它怎么改
```

## 安装

```sh
git clone https://github.com/whoamihappyhacking/di.git
cd di && go build -o di . && ./di install
```

会装到 `~/.local/bin/d` 和 `~/.local/bin/di`，确保这个目录在 `PATH` 里。`di` 的选择器需要 [fzf](https://github.com/junegunn/fzf)。

## 问答

**和 tmux、screen 有什么不同？**

di 不管窗口和分屏，一个命令就是一个会话，也没有前缀键要学。会话里的输出原样直达你的终端，不经过二次渲染，所以滚屏、鼠标选择、颜色和快捷键都和直接运行一致。

**机器重启后能恢复到什么程度？**

命令和目录。重启后打开 `di`，没跑完的会话显示为“⟲ 已中断”：按 `Enter` 在原目录用原参数重新运行，按 `Ctrl-O` 在原目录开一个 shell。进程内存和画面回不来，智能体的对话要靠它自己的续接参数，比如 `claude --continue`。

**在会话里再运行 `d` 或 `di` 会套娃吗？**

不会，外层终端会直接切到目标会话。

**怎么结束一个会话？**

在 `di` 里选中它按 `Ctrl-X`，或者运行 `d --kill <名字>`，名字见 `d --list` 的最后一列。命令在你离开时退出的话，会话会留着最后的画面，等你回来看一眼再清掉。

**快捷键和我的程序冲突了怎么办？**

用环境变量换掉：`D_DETACH='^B'` 换离开键，`D_SWITCH='^G'` 换切换键。

**支持哪些系统，需要什么依赖？**

Linux 和 macOS。从源码构建需要 Go 1.25 以上；`di` 的选择器依赖 fzf，`d` 启动会话和 `d --list` 不需要。

完整的命令、按键和状态说明见 `d --help`。

## 开发

```sh
go test ./...                                         # 含驱动真实二进制的端到端测试，加 -short 跳过
python3 -m http.server 7789 --directory website       # 本地预览项目主页
```

推送 `website/` 的改动到 `main` 后，GitHub Actions 会自动部署项目主页。
