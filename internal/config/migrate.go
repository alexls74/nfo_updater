// internal/config/migrate.go
package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"nfo_updater/internal/version"
)

//go:embed config.conf
var defaultConfigTemplate string

var reKeyLine = regexp.MustCompile(`(?m)^([A-Z_][A-Z0-9_]*)=(.*)$`)
var reVersionLine = regexp.MustCompile(`(?m)^VERSION=.*$`)

// ErrConfigCreated — сентинел: файла конфига не было, только что создан
// шаблон по умолчанию. main.go должен вывести дружелюбное сообщение
// с путём к файлу и выйти, не пытаясь сразу стартовать.
var ErrConfigCreated = fmt.Errorf("default config created, please edit it and restart")

// EnsureConfig гарантирует, что по пути path лежит конфиг актуальной версии.
func EnsureConfig(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := writeDefaultConfig(path); err != nil {
			return "", fmt.Errorf("write default config: %w", err)
		}
		return "", ErrConfigCreated
	}
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}

	content := string(raw)
	fileVersion := extractVersion(content)

	switch compareVersions(fileVersion, version.Version) {
	case 0:
		return content, nil
	case 1:
		return "", fmt.Errorf(
			"config VERSION (%s) is newer than this binary (%s): downgrading the binary over a newer config is not supported, install a matching or newer release",
			fileVersion, version.Version,
		)
	}

	migrated, dropped := transplantValues(defaultConfigTemplate, content)
	migrated = setVersion(migrated, version.Version)

	if err := os.WriteFile(path, []byte(migrated), 0o644); err != nil {
		return "", fmt.Errorf("write migrated config: %w", err)
	}
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "[CONFIG_MIGRATION] the following keys had values in your old config but no longer exist in version %s and were dropped: %s\n",
			version.Version, strings.Join(dropped, ", "))
	}
	return migrated, nil
}

func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := setVersion(defaultConfigTemplate, version.Version)
	return os.WriteFile(path, []byte(content), 0o644)
}

func transplantValues(template, oldContent string) (result string, dropped []string) {
	oldValues := make(map[string]string)
	for _, m := range reKeyLine.FindAllStringSubmatch(oldContent, -1) {
		key, val := m[1], m[2]
		if val != "" {
			oldValues[key] = val
		}
	}

	usedKeys := make(map[string]bool)
	result = reKeyLine.ReplaceAllStringFunc(template, func(line string) string {
		m := reKeyLine.FindStringSubmatch(line)
		key := m[1]
		if val, ok := oldValues[key]; ok {
			usedKeys[key] = true
			return key + "=" + val
		}
		return line
	})

	for key := range oldValues {
		if !usedKeys[key] {
			dropped = append(dropped, key)
		}
	}
	return result, dropped
}

func extractVersion(content string) string {
	m := reVersionLine.FindString(content)
	if m == "" {
		return "0.0.0"
	}
	return strings.TrimPrefix(m, "VERSION=")
}

func setVersion(content, v string) string {
	block := "\n# ------------------------------------------------------------------------------\n" +
		"# INTERNAL — managed automatically, do not edit\n" +
		"# ------------------------------------------------------------------------------\n" +
		"VERSION=" + v + "\n"

	if reVersionLine.MatchString(content) {
		return reVersionLine.ReplaceAllLiteralString(content, "VERSION="+v)
	}
	return strings.TrimRight(content, "\n") + "\n" + block
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		na, nb := versionPart(pa, i), versionPart(pb, i)
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionPart(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n := 0
	for _, c := range parts[i] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
