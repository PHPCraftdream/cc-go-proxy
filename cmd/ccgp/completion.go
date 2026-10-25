package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

// Shell completion. The scripts are thin: they pass the command line to the
// hidden `ccgp --complete`, which decides the candidates in Go (testable, one
// source: the command table plus the configured names).

// emptyWord stands for an empty word being completed: Windows PowerShell 5.1
// drops empty arguments of native commands.
const emptyWord = "__ccgp_empty__"

const completionUsage = `usage: ccgp --completion bash|zsh|powershell

  bash        source <(ccgp --completion bash)
  zsh         source <(ccgp --completion zsh)
  powershell  ccgp --completion powershell | Out-String | Invoke-Expression

Put the line in your shell profile to keep it.`

func completionCmd(args []string) error {
	if len(args) != 1 {
		return errors.New(completionUsage)
	}
	switch args[0] {
	case "bash":
		fmt.Print(bashScript)
	case "zsh":
		fmt.Print(zshScript)
	case "powershell":
		fmt.Print(powershellScript)
	default:
		return errors.New(completionUsage)
	}
	return nil
}

const bashScript = `_ccgp_complete() {
  local IFS=$'\n'
  COMPREPLY=($("${COMP_WORDS[0]}" --complete "${COMP_WORDS[@]:0:COMP_CWORD+1}" 2>/dev/null))
}
complete -F _ccgp_complete ccgp ccgpy
`

const zshScript = `#compdef ccgp ccgpy
_ccgp_complete() {
  local -a candidates
  candidates=("${(@f)$("$words[1]" --complete "${(@)words[1,CURRENT]}" 2>/dev/null)}")
  candidates=(${candidates:#})
  (( ${#candidates} )) && compadd -- "${candidates[@]}"
}
if (( $+functions[compdef] )); then compdef _ccgp_complete ccgp ccgpy; fi
`

const powershellScript = `Register-ArgumentCompleter -Native -CommandName ccgp, ccgpy -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $words = @($commandAst.CommandElements | ForEach-Object { $_.ToString() })
    if ($wordToComplete -eq '') { $words += '` + emptyWord + `' }
    & $words[0] --complete @words 2>$null | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`

// completionNames supplies the configured names, loaded only when needed.
type completionNames struct {
	profiles  func() []string
	providers func() []string
}

// completeCmd implements the hidden `--complete <prog> <word>...`: prints the
// candidates for the last word, one per line. It never fails and never
// creates a config, so a broken setup cannot spew errors into the prompt.
func completeCmd(args []string) error {
	if len(args) == 0 {
		return nil
	}
	for _, c := range completeWords(args, configNames()) {
		fmt.Println(c)
	}
	return nil
}

// configNames reads profile and provider names from the config, once and
// quietly: any error gives no names.
func configNames() completionNames {
	var (
		once sync.Once
		cfg  *config.Config
	)
	get := func() *config.Config {
		once.Do(func() {
			if c, err := config.Load(); err == nil {
				cfg = c
			}
		})
		return cfg
	}
	return completionNames{
		profiles: func() []string {
			if c := get(); c != nil {
				return sortedKeys(c.Profiles)
			}
			return nil
		},
		providers: func() []string {
			if c := get(); c != nil {
				return sortedKeys(c.Providers)
			}
			return nil
		},
	}
}

// completeWords returns the sorted candidates for the last of words, where
// words[0] is the program (ccgp or ccgpy) as the shell typed it.
func completeWords(words []string, n completionNames) []string {
	prog := strings.ToLower(strings.TrimSuffix(filepath.Base(words[0]), ".exe"))
	rest := append([]string(nil), words[1:]...)
	if len(rest) == 0 {
		return nil
	}
	cur := rest[len(rest)-1]
	if cur == emptyWord {
		cur = ""
	}
	prior := rest[:len(rest)-1]

	var all []string
	switch {
	case prog == "ccgpy":
		// ccgpy <profile> <claude args...>
		if len(prior) == 0 {
			all = call(n.profiles)
		}
	case len(prior) == 0:
		all = append(commandNames(), call(n.profiles)...)
	case !strings.HasPrefix(prior[0], "-"):
		// A profile run: the rest belongs to claude.
	default:
		all = commandCandidates(prior[0], prior[1:], n)
	}

	seen := map[string]bool{}
	var out []string
	for _, c := range all {
		if strings.HasPrefix(c, cur) && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func call(f func() []string) []string {
	if f == nil {
		return nil
	}
	return f()
}

// commandNames lists the visible commands of the table.
func commandNames() []string {
	var out []string
	for _, c := range commands() {
		if !c.hidden {
			out = append(out, c.name)
		}
	}
	return out
}

// commandCandidates offers what can follow `ccgp <cmd> <sub...>`.
func commandCandidates(cmd string, sub []string, n completionNames) []string {
	prev := ""
	if len(sub) > 0 {
		prev = sub[len(sub)-1]
	}
	switch cmd {
	case "--status":
		return []string{"--json"}
	case "--logs":
		return []string{"-f", "-n"}
	case "--serve":
		return []string{"-d", "-s"}
	case "--doctor":
		return []string{"--offline", "--json"}
	case "--update":
		return []string{"--check"}
	case "--config":
		if len(sub) == 0 {
			return []string{"reset"}
		}
	case "--completion":
		if len(sub) == 0 {
			return []string{"bash", "zsh", "powershell"}
		}
	case "--login":
		return append(call(n.providers), "--device")
	case "--logout":
		return append(call(n.providers), "--yes")
	case "--models":
		return append(call(n.profiles), "apply", "--json", "-v", "--verbose")
	case "--which":
		if len(sub) == 0 {
			return call(n.profiles)
		}
		return []string{"--json"}
	case "--import", "--export":
		if len(sub) == 0 {
			return call(n.profiles)
		}
	case "--send":
		if len(sub) < 2 {
			return append(call(n.profiles), "claude")
		}
	case "--profile":
		return profileCandidates(sub, prev, n)
	}
	return nil
}

// profileCandidates completes `ccgp --profile <add|list|rm> ...`.
func profileCandidates(sub []string, prev string, n completionNames) []string {
	if len(sub) == 0 {
		return []string{"add", "list", "rm"}
	}
	switch sub[0] {
	case "add":
		switch prev {
		case "--like":
			return call(n.profiles)
		case "--provider":
			return call(n.providers)
		case "--type":
			return []string{"codex", "codex_api", "anthropic_compat"}
		}
		return []string{"--like", "--provider", "--type", "--base-url", "--model", "--window"}
	case "list":
		return []string{"--json"}
	case "rm":
		return append(call(n.profiles), "--yes", "--purge")
	}
	return nil
}
