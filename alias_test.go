package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func testBashAliases(t *testing.T, startup string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	rc := filepath.Join(dir, "bashrc")
	if err := os.WriteFile(rc, []byte(startup), 0600); err != nil {
		t.Fatal(err)
	}
	// Use an isolated rc file without changing the user's HOME or loading their
	// startup commands. The wrapper's name exercises bash shell detection.
	shell := filepath.Join(dir, "bash")
	script := "#!/bin/sh\nexec " + shellQuote(bash) + " --noprofile --rcfile " + shellQuote(rc) + ` "$@"` + "\n"
	if err := os.WriteFile(shell, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
}

func runAliasCommand(t *testing.T, args []string) string {
	t.Helper()
	expanded := expandCommandAlias(args)
	output, err := exec.Command(expanded[0], expanded[1:]...).Output()
	if err != nil {
		t.Fatalf("run alias: %v", err)
	}
	return string(output)
}

func TestAliasesRunWithShellSemantics(t *testing.T) {
	for _, test := range []struct {
		name    string
		startup string
		args    []string
		want    string
	}{
		{
			name:    "literal arguments",
			startup: "alias app=" + shellQuote(`printf '%s\n' fixed`) + "\n",
			args:    []string{"app", "two words", "$(printf unexpected)", "'quoted'", "$PATH", ""},
			want:    "fixed\ntwo words\n$(printf unexpected)\n'quoted'\n$PATH\n\n",
		},
		{
			name:    "environment assignment",
			startup: "alias app=" + shellQuote(`D_ALIAS_VALUE=ready sh -c 'printf "%s\n" "$D_ALIAS_VALUE"'`) + "\n",
			args:    []string{"app"},
			want:    "ready\n",
		},
		{
			name:    "self alias expands once",
			startup: "alias echo='echo tagged'\n",
			args:    []string{"echo", "tail"},
			want:    "tagged tail\n",
		},
		{
			name:    "nested aliases",
			startup: "alias inner=" + shellQuote(`printf '%s\n'`) + "\nalias app='inner fixed'\n",
			args:    []string{"app", "extra"},
			want:    "fixed\nextra\n",
		},
		{
			name:    "pipeline",
			startup: "alias app=" + shellQuote(`printf '%s\n' fixed | tr a-z A-Z`) + "\n",
			args:    []string{"app"},
			want:    "FIXED\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			testBashAliases(t, test.startup)
			if got := runAliasCommand(t, test.args); got != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAliasesRefreshWhenSourcedFileChanges(t *testing.T) {
	file := filepath.Join(t.TempDir(), "aliases")
	writeAlias := func(value string) {
		t.Helper()
		line := "alias app=" + shellQuote(`printf '%s\n' `+value) + "\n"
		if err := os.WriteFile(file, []byte(line), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeAlias("old")
	testBashAliases(t, ". "+shellQuote(file)+"\n")
	if got := runAliasCommand(t, []string{"app"}); got != "old\n" {
		t.Fatalf("initial output = %q", got)
	}
	writeAlias("new")
	if got := runAliasCommand(t, []string{"app"}); got != "new\n" {
		t.Fatalf("output after sourced file changed = %q", got)
	}
}

func TestUnknownCommandDoesNotUseShellWrapper(t *testing.T) {
	testBashAliases(t, "alias app='echo app'\n")
	args := []string{"unknown-app", "a b", "$(literal)"}
	if got := expandCommandAlias(args); !reflect.DeepEqual(got, args) {
		t.Fatalf("non-alias command changed: %q", got)
	}
	if got := expandAliasArgs(nil, nil, os.Getenv("SHELL")); len(got) != 0 {
		t.Fatalf("empty command changed: %q", got)
	}
}

func TestAliasRunsInsidePTYSession(t *testing.T) {
	testBashAliases(t, "alias app="+shellQuote(`D_ALIAS_VALUE=ready sh -c 'printf "%s\n" "$D_ALIAS_VALUE"'`)+"\n")
	output := runServerAndRead(t, expandCommandAlias([]string{"app"}))
	if !bytes.Contains(output, []byte("ready\r\n")) {
		t.Fatal("alias output was not replayed from its PTY session")
	}
}
