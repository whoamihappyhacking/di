package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Restore records let a session be started again after a reboot. While a
// session's command runs, its server keeps a record under the state
// directory, which unlike the runtime directory holding the sockets survives a
// reboot. The record goes away when the command ends or the session is
// killed, so a record whose socket is gone marks a session that was
// interrupted: the machine went down, or its server was killed.

type restoreRecord struct {
	sessionMeta
	Sock   string `json:"sock,omitempty"`
	BootID string `json:"boot_id,omitempty"`
	// Adopted records are written by clients for sessions whose server is
	// from an older build and does not remove the record when the command
	// ends; one left behind in the same boot says nothing.
	Adopted bool `json:"adopted,omitempty"`
}

const (
	// The server refreshes its record's mtime, so a record left behind tells
	// roughly when the session went away.
	restoreHeartbeat = time.Minute
	// A shutdown signal reaching the server this soon after the command
	// exited means the command went down with the system.
	restoreGrace    = time.Second
	interruptedKeep = 7 * 24 * time.Hour
)

func restoreDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "di", "restore"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "di", "restore"), nil
}

func restorePath(name string) (string, error) {
	dir, err := restoreDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// isRestoreRecord tells picker entries for interrupted sessions, which carry
// their record path, from live ones, which carry a socket path.
func isRestoreRecord(path string) bool {
	return strings.HasSuffix(path, ".json")
}

// writeRestoreRecord writes sock's record and returns its path and content.
func writeRestoreRecord(sock string, meta sessionMeta, adopted bool) (string, []byte, error) {
	path, err := restorePath(sessionName(sock))
	if err != nil {
		return "", nil, err
	}
	meta.Name = sessionName(sock)
	data, err := json.MarshalIndent(restoreRecord{sessionMeta: meta, Sock: sock, BootID: bootID(), Adopted: adopted}, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return path, data, writeFileAtomic(path, data)
}

// writeFileAtomic replaces path, so a reader never sees half a record.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readRestoreRecord(path string) (restoreRecord, error) {
	var rec restoreRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	if len(rec.Command) == 0 {
		return rec, fmt.Errorf("d: restore record without a command: %s", path)
	}
	if rec.Name == "" {
		rec.Name = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	return rec, nil
}

// adoptSession makes sure a session run by an older di build has a record,
// and refreshes it the way a current server's heartbeat would.
func adoptSession(sock string, meta sessionMeta) {
	if len(meta.Command) == 0 {
		return
	}
	path, err := restorePath(sessionName(sock))
	if err != nil {
		return
	}
	if _, err := os.Stat(path); err == nil {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return
	}
	_, _, _ = writeRestoreRecord(sock, meta, true)
}

// clearStaleSession removes the socket a server left behind when it died
// without cleaning up: it was killed, or the machine went down, while its
// command ran. A server whose command ends removes its socket itself, so this
// marks even a record adopted from an older build as an interruption.
func clearStaleSession(sock string) {
	if path, err := restorePath(sessionName(sock)); err == nil {
		if rec, err := readRestoreRecord(path); err == nil && rec.Adopted {
			rec.Adopted = false
			if data, err := json.MarshalIndent(rec, "", "  "); err == nil {
				_ = writeFileAtomic(path, data)
			}
		}
	}
	removeSessionFiles(sock)
}

// forgetAdopted drops the record of an older build's session whose command
// has ended.
func forgetAdopted(sock string) {
	path, err := restorePath(sessionName(sock))
	if err != nil {
		return
	}
	if rec, err := readRestoreRecord(path); err == nil && rec.Adopted {
		_ = os.Remove(path)
	}
}

// collectInterrupted lists the sessions that were running when they went away
// without ending. Live sessions must have been queried first, so sockets of
// servers that are gone have already been cleared.
func collectInterrupted() []sessionEntry {
	dir, err := restoreDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	boot := bootID()
	var list []sessionEntry
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		rec, err := readRestoreRecord(path)
		if err != nil {
			_ = os.Remove(path)
			continue
		}
		if rec.Sock != "" && isSocket(rec.Sock) {
			continue // still running, or a server that did not answer in time
		}
		sameBoot := boot != "" && rec.BootID == boot
		if time.Since(info.ModTime()) > interruptedKeep || (rec.Adopted && sameBoot) {
			_ = os.Remove(path)
			continue
		}
		reason := "系统重启前在运行"
		if sameBoot {
			reason = "会话进程意外结束"
		}
		list = append(list, sessionEntry{
			Sock:   path,
			Meta:   rec.sessionMeta,
			Status: sessionStatus{State: "interrupted", Summary: reason, LastOutput: info.ModTime()},
		})
	}
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].Status.LastOutput.After(list[j].Status.LastOutput)
	})
	return list
}

// bootID identifies the running boot, so a record can tell a reboot from a
// server that was killed. It is empty where the platform does not say.
var bootID = sync.OnceValue(func() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return strings.TrimSpace(string(b))
	}
	for _, name := range []string{"kern.bootsessionuuid", "kern.boottime"} {
		if out, err := exec.Command("sysctl", "-n", name).Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
})

// openPicked turns a picker choice into the socket of a session to show:
// a live session as is; an interrupted one is started again; with inDir, a
// shell is started in the chosen session's directory.
func openPicked(target string, inDir bool) (string, error) {
	var meta sessionMeta
	if isRestoreRecord(target) {
		rec, err := readRestoreRecord(target)
		if err != nil {
			return "", fmt.Errorf("di: cannot read interrupted session: %w", err)
		}
		meta = rec.sessionMeta
	} else {
		if !inDir {
			return target, nil
		}
		meta = readSessionMeta(target)
	}
	args := meta.Command
	if inDir {
		args = []string{loginShell()}
	}
	if meta.PWD == "" {
		return "", fmt.Errorf("di: %s has no recorded directory", cleanSessionName(meta.Name, target))
	}
	if info, err := os.Stat(meta.PWD); err != nil || !info.IsDir() {
		return "", fmt.Errorf("di: directory no longer exists: %s", meta.PWD)
	}
	sock, err := startSessionIn(meta.PWD, args)
	if err != nil {
		return "", err
	}
	if isRestoreRecord(target) {
		_ = os.Remove(target)
	}
	return sock, nil
}

func loginShell() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return "/bin/sh"
	}
	// Prefer the bare name for a readable session name when PATH finds it.
	if found, err := exec.LookPath(filepath.Base(shell)); err == nil && samePath(found, shell) {
		return filepath.Base(shell)
	}
	return shell
}

// samePath also matches through symlinks, as /bin/zsh and /usr/bin/zsh where
// /bin links to /usr/bin.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// startSessionIn starts a session the way `d` would when run from dir, so
// relative commands, aliases and the recorded directory all resolve there.
func startSessionIn(dir string, args []string) (string, error) {
	prev, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if err := os.Chdir(dir); err != nil {
		return "", err
	}
	defer func() { _ = os.Chdir(prev) }()
	return startSession(args)
}

// resolveInterrupted finds an interrupted session by name the way
// resolveSession finds a live one, and returns its record path.
func resolveInterrupted(arg string) (string, error) {
	list, err := collectSessions("")
	if err != nil {
		return "", err
	}
	paths := map[string]string{}
	var names []string
	for _, e := range list {
		if e.Status.State == "interrupted" {
			names = append(names, e.name())
			paths[e.name()] = e.Sock
		}
	}
	name, err := matchSessionName(names, arg)
	if err != nil {
		return "", err
	}
	return paths[name], nil
}

func restoreByName(arg string, inDir bool) error {
	path, err := resolveInterrupted(arg)
	if err != nil {
		if errors.Is(err, errNoSession) {
			return fmt.Errorf("d: no interrupted session matches %q", arg)
		}
		return err
	}
	sock, err := openPicked(path, inDir)
	if err != nil {
		return err
	}
	return enterSession(sock)
}
