package localruntime

import "strings"

// launchd services can start without a locale. zsh then measures Unicode
// prompts as US-ASCII and reports the wrong cursor column.
func shellCharacterLocaleDefault(env []string, goos string) string {
	if goos != "darwin" {
		return ""
	}
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		if value == "" {
			continue
		}
		switch key {
		case "LANG", "LC_ALL", "LC_CTYPE":
			return ""
		}
	}
	return "UTF-8"
}
