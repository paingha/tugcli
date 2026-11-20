package session

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed hooks/*.tmpl
var hookFS embed.FS

// Shell is a supported interactive shell.
type Shell string

// Supported shells.
const (
	Zsh        Shell = "zsh"
	Bash       Shell = "bash"
	Fish       Shell = "fish"
	PowerShell Shell = "powershell"
)

// Shells lists the supported shells.
func Shells() []Shell { return []Shell{Zsh, Bash, Fish, PowerShell} }

// ParseShell accepts a shell name, including common aliases.
func ParseShell(name string) (Shell, error) {
	switch strings.ToLower(name) {
	case "zsh":
		return Zsh, nil
	case "bash", "sh":
		return Bash, nil
	case "fish":
		return Fish, nil
	case "powershell", "pwsh", "ps":
		return PowerShell, nil
	default:
		return "", fmt.Errorf("unsupported shell %q (supported: zsh, bash, fish, powershell)", name)
	}
}

// Quote quotes s as a single literal word for the shell.
func (sh Shell) Quote(s string) string {
	switch sh {
	case Fish:
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
	case PowerShell:
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	default:
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
}

// Hook returns the init script for the shell. bin is the tug command the
// script calls.
func Hook(sh Shell, bin string) (string, error) {
	tmpl, err := template.ParseFS(hookFS, "hooks/"+string(sh)+".tmpl")
	if err != nil {
		return "", fmt.Errorf("no hook for shell %q: %w", sh, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Bin string }{Bin: sh.Quote(bin)}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ExportScript returns shell statements that export the variables, in order.
func ExportScript(sh Shell, vars [][2]string) string {
	var b strings.Builder
	for _, kv := range vars {
		name, value := kv[0], kv[1]
		switch sh {
		case Fish:
			fmt.Fprintf(&b, "set -gx %s %s\n", name, sh.Quote(value))
		case PowerShell:
			fmt.Fprintf(&b, "$env:%s = %s\n", name, sh.Quote(value))
		default:
			fmt.Fprintf(&b, "export %s=%s\n", name, sh.Quote(value))
		}
	}
	return b.String()
}
