package runner

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const maximumEnvironmentLine = 1024 * 1024

var environmentKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func environmentForJob(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	overrides, err := readEnvironmentFile(path)
	if err != nil {
		return nil, err
	}
	return mergeEnvironment(os.Environ(), overrides), nil
}

func readEnvironmentFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open environment file: %w", err)
	}
	defer file.Close()

	var values []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maximumEnvironmentLine)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, rawValue, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !environmentKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("environment file line %d: expected KEY=VALUE", lineNumber)
		}
		value, err := parseEnvironmentValue(strings.TrimSpace(rawValue))
		if err != nil {
			return nil, fmt.Errorf("environment file line %d: %w", lineNumber, err)
		}
		values = append(values, key+"="+value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read environment file: %w", err)
	}
	return values, nil
}

func parseEnvironmentValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	switch value[0] {
	case '\'':
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		return value[1 : len(value)-1], nil
	case '"':
		if len(value) < 2 || value[len(value)-1] != '"' {
			return "", fmt.Errorf("unterminated double-quoted value")
		}
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted value")
		}
		return decoded, nil
	default:
		return value, nil
	}
}

func mergeEnvironment(base, overrides []string) []string {
	result := append([]string(nil), base...)
	positions := make(map[string]int, len(result))
	for index, entry := range result {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			positions[key] = index
		}
	}
	for _, entry := range overrides {
		key, _, _ := strings.Cut(entry, "=")
		if index, exists := positions[key]; exists {
			result[index] = entry
			continue
		}
		positions[key] = len(result)
		result = append(result, entry)
	}
	return result
}
