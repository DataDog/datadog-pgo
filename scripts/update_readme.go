package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	modeF := flag.String("mode", "write", "update readme")
	flag.Parse()

	help, err := helpText()
	if err != nil {
		return fmt.Errorf("failed to get help text: %w", err)
	}

	readme, err := readmeMarkdown()
	if err != nil {
		return fmt.Errorf("failed to get README.md: %w", err)
	}

	newReadme := updateReadme(readme, help)
	switch *modeF {
	case "write":
		return os.WriteFile("README.md", []byte(newReadme), 0644)
	case "check":
		if newReadme != readme {
			return fmt.Errorf("README.md is out of date")
		}
	case "print":
		fmt.Fprintf(os.Stdout, "%s", newReadme)
	}
	return nil
}

func updateReadme(readme string, help string) string {
	newReadme := replaceSection(readme, "scripts/update_readme.go", "```\n"+help+"```\n")
	newReadme = strings.TrimSpace(newReadme) + "\n"
	return newReadme
}

func replaceSection(input, section, value string) string {
	marker := fmt.Sprintf("<!-- %s -->", section)
	return regexp.
		MustCompile("(?s)"+marker+".*?"+marker).
		ReplaceAllString(input, marker+"\n"+value+marker)
}

func readmeMarkdown() (string, error) {
	data, err := os.ReadFile("README.md")
	return string(data), err
}

func helpText() (string, error) {
	buf := &bytes.Buffer{}
	c := exec.Command("go", "run", ".", "-h")
	c.Stdout = buf
	c.Stderr = buf
	if err := c.Run(); err != nil {
		return "", err
	}
	return buf.String(), nil
}
