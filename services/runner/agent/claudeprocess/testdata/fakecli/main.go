// Command fakecli is synthetic process evidence, not Claude or a provider client.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// main exercises controlled version, output, environment and process-tree behaviors.
func main() {
	if len(os.Args) == 2 && os.Args[1] == "--child" {
		time.Sleep(time.Hour)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			os.Exit(3)
		}
		if strings.Contains(filepath.Base(os.Args[0]), "wrong") {
			fmt.Println("2.1.284 (Claude Code)")
			return
		}
		if strings.Contains(filepath.Base(os.Args[0]), "probe-hang") {
			time.Sleep(time.Hour)
			return
		}
		fmt.Println("2.1.285 (Claude Code)")
		return
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	home := os.Getenv("HOME")
	if key != "synthetic-key" || home == "" {
		os.Exit(3)
	}
	info, err := os.Stat(home)
	if err != nil || info.Mode().Perm() != 0700 {
		os.Exit(3)
	}
	allowed := map[string]bool{"HOME": true, "TMPDIR": true, "XDG_CONFIG_HOME": true, "PATH": true, "LANG": true, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": true, "ANTHROPIC_API_KEY": true}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !allowed[name] {
			os.Exit(3)
		}
	}
	if os.Getenv("TMPDIR") != home || os.Getenv("XDG_CONFIG_HOME") != filepath.Join(home, ".config") {
		os.Exit(3)
	}
	for _, arg := range os.Args[1:] {
		if strings.Contains(arg, "synthetic-key") || strings.Contains(arg, "UNTRUSTED") {
			os.Exit(3)
		}
	}
	for _, flag := range []string{"--bare", "--print", "--verbose", "--strict-mcp-config", "--no-session-persistence"} {
		if !contains(flag) {
			os.Exit(3)
		}
	}
	for flag, want := range map[string]string{"--input-format": "text", "--output-format": "stream-json", "--tools": "", "--disallowedTools": "*", "--permission-mode": "dontAsk", "--setting-sources": "", "--settings": "{}", "--mcp-config": `{"mcpServers":{}}`} {
		got, ok := value(flag)
		if !ok || got != want {
			os.Exit(3)
		}
	}
	for _, arg := range os.Args[1:] {
		if strings.Contains(arg, "skip-permissions") || arg == "bypassPermissions" {
			os.Exit(3)
		}
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(3)
	}
	var input struct {
		Task         string `json:"task"`
		Instructions string `json:"agent_instructions"`
	}
	if json.Unmarshal(raw, &input) != nil || !strings.Contains(input.Task, "UNTRUSTED") || !strings.Contains(input.Instructions, "UNTRUSTED") {
		os.Exit(3)
	}
	model, _ := value("--model")
	switch model {
	case "hang":
		time.Sleep(time.Hour)
		return
	case "descendant":
		child := exec.Command(os.Args[0], "--child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if child.Start() != nil {
			os.Exit(3)
		}
		message(fmt.Sprintf("parent=%d child=%d", os.Getpid(), child.Process.Pid))
		// Exit the leader while the child still holds stdout open.
		return
	case "flood":
		for i := 0; i < 1024; i++ {
			fmt.Fprint(os.Stderr, strings.Repeat("SYNTHETIC_SECRET", 4096))
		}
	case "malformed":
		fmt.Println("SYNTHETIC_SECRET invalid JSON")
		return
	case "empty":
		return
	}
	text := "verified"
	if model == "home" || model == "unclean-home" {
		text = home
	}
	if model == "unclean-home" {
		blocked := filepath.Join(home, "blocked")
		if os.Mkdir(blocked, 0700) != nil || os.WriteFile(filepath.Join(blocked, "file"), []byte("synthetic"), 0600) != nil || os.Chmod(blocked, 0) != nil {
			os.Exit(3)
		}
	}
	message(text)
	if model == "backpressure" {
		time.Sleep(time.Hour)
		return
	}
	result(model == "failure")
	if model == "nonzero" {
		os.Exit(9)
	}
	if model == "exit-hang" {
		_ = os.Stdout.Close()
		time.Sleep(time.Hour)
	}
}

// contains checks boolean CLI switches without interpreting user prompt data.
func contains(want string) bool {
	for _, arg := range os.Args[1:] {
		if arg == want {
			return true
		}
	}
	return false
}

// value preserves empty option values so removing --tools is detectable.
func value(want string) (string, bool) {
	for i, arg := range os.Args[1:] {
		if arg == want && i+2 < len(os.Args) {
			return os.Args[i+2], true
		}
	}
	return "", false
}

// message emits a synthetic complete assistant record.
func message(text string) {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "assistant", "session_id": "synthetic-session", "uuid": "10000000-0000-4000-8000-000000000001", "message": map[string]any{"id": "synthetic-api-message", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
}

// result emits a synthetic terminal result, with no provider/network invocation.
func result(failed bool) {
	subtype := "success"
	if failed {
		subtype = "error_during_execution"
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "session_id": "synthetic-session", "subtype": subtype, "is_error": failed, "permission_denials": []any{}, "usage": map[string]int{"input_tokens": 2, "output_tokens": 3, "cache_read_input_tokens": 5, "cache_creation_input_tokens": 7}})
}
